package libkrun

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"sync"
	"time"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/link/ethernet"
	"gvisor.dev/gvisor/pkg/tcpip/network/arp"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"

	"github.com/hyper-swe/mgit/internal/guestboot"
	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/egress"
)

// Virtual network the guest's NIC sits on. RFC1918 space, host-assigned; the
// guest reaches nothing except through the gateway address.
const (
	guestIP   = "10.0.2.15"
	gatewayIP = "10.0.2.2"
	guestMTU  = 1500
	gwNICID   = 1
	// dnsPort is where the guest finds the host-side resolver. It is the ONLY
	// UDP endpoint bound in the stack, so every other guest datagram is dropped.
	dnsPort = 53
	// gwPrefixLen puts the gateway and the guest on ONE link, which is what
	// lets the host dial INTO the guest (SEC-09 publish). A /32 would leave
	// the guest off-link and inbound connections unroutable.
	gwPrefixLen   = 24
	gwDialTimeout = 10 * time.Second
	// gwMaxInFlight bounds half-open connection attempts a hostile guest can
	// hold against the stack (T7 resource abuse).
	gwMaxInFlight = 512
)

// gatewayMAC is the host end's link address on the virtual wire.
var gatewayMAC = tcpip.LinkAddress("\x02\x00\x00\x00\x00\x02")

// guestNetworkFor is the descriptor the GUEST needs to use this wire: its own
// address, the link prefix, the default gateway, and the resolver.
//
// It is derived from the constants directly above rather than restated
// anywhere, so the two ends of the wire cannot drift. Nothing else configures
// the guest's NIC — mgit-guest is PID 1 in the sandbox, so no init, dhclient
// or NetworkManager ever runs — and without this descriptor the guest boots
// with eth0 present but UNADDRESSED and with no default route. That is
// MGIT-68: it fails every flow, allowed or denied, which a test asserting
// only that denied destinations are denied cannot distinguish from working
// enforcement.
//
// none mode gets NO descriptor: its NIC exists only to keep libkrun off its
// fail-open TSI fallback and is backed by a discard socket, so there is no
// gateway to point a guest at. Refs: MGIT-68, FR-17.7, SEC-07, ADR-010
func guestNetworkFor(mode string) guestboot.GuestNetwork {
	if mode == model.NetworkModeNone {
		return guestboot.GuestNetwork{}
	}
	return guestboot.GuestNetwork{
		IP:        guestIP,
		PrefixLen: gwPrefixLen,
		Gateway:   gatewayIP,
		// The resolver is the gateway: serveDNS binds the host-side pinning
		// resolver at gatewayIP:dnsPort, and it is the ONLY UDP endpoint in
		// the stack, so any other nameserver the guest inherited is
		// unreachable by construction. Refs: SEC-07
		DNS: gatewayIP,
	}
}

// flowAuthorizer is the egress policy seam. *egress.Authorizer satisfies it;
// the gateway holds only this interface so the policy stays owned by the
// egress package and the data path can be tested against a stub.
// Refs: SEC-04, FR-17.8
type flowAuthorizer interface {
	Authorize(ctx context.Context, f egress.Flow) (egress.Decision, error)
}

// dnsResolver serves the guest's DNS over a PacketConn the gateway binds
// inside the netstack. *egress.DNSServer satisfies it, so the host-side
// allowlist-gated, pinned resolution (SEC-07) is reused unchanged rather than
// reimplemented here. Refs: SEC-04, SEC-07, FR-17.8
type dnsResolver interface {
	ServeUDP(ctx context.Context, pc net.PacketConn) error
}

// netDeps are the per-VM collaborators the host-side network wiring needs.
//
// They travel together because they are acquired together and every one of
// them is per-launch: the policy that decides a flow, the resolver that pins
// a name, the transport that carries an approved connection, and the logger
// that records what happened. Passing them as one value also keeps the
// constructors inside CLAUDE.md's parameter budget as the set grows.
// Refs: SEC-04, FR-17.8, MGIT-61.9
type netDeps struct {
	auth   flowAuthorizer
	dns    dnsResolver
	dial   egress.DialFunc
	logger *slog.Logger
}

// withDefaults fills the optional collaborators. A nil logger is discarded
// rather than left nil: this runs in the VM child, where a nil-pointer
// dereference on the data path kills the sandbox.
func (d netDeps) withDefaults() netDeps {
	if d.dial == nil {
		d.dial = egress.HostDial
	}
	if d.logger == nil {
		d.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return d
}

// netGateway is the host end of an allowlist/open-mode NIC: a USERSPACE
// TCP/IP stack that terminates every connection the guest opens and admits
// only those the authorizer allows.
//
// Why a userspace stack rather than the tap+iptables engine the firecracker
// backend uses: libkrun's virtio-net backing is a unixgram socket carrying
// ethernet frames, which an L4 proxy cannot serve. Terminating the guest's
// TCP in-process also removes the tap device, the host firewall mutation and
// the root requirement — and makes egress enforcement testable without a VM.
//
// The guest's only TCP path out is handleForward below.
//
// UDP POSTURE (be precise — this shapes what allowlist mode means): exactly
// ONE UDP endpoint is bound, the DNS resolver at gatewayIP:53 (serveDNS), and
// there is deliberately NO general UDP forwarder. Every other guest datagram
// therefore has no listener and is dropped, so UDP cannot become an
// unauthorized egress path — there is no per-flow authorization for it to
// pass through. Name resolution goes through the host resolver, which PINS
// the address (SEC-07), which is what lets allowlisted NAMES work rather than
// only IP/CIDR entries. Refs: FR-17.7, FR-17.8, SEC-04, SEC-07, SEC-10, ADR-010
type netGateway struct {
	conn   *net.UnixConn
	path   string
	ch     *channel.Endpoint
	stack  *stack.Stack
	auth   flowAuthorizer
	dns    dnsResolver
	dial   egress.DialFunc
	logger *slog.Logger

	// cancel stops the DNS server (and anything else scoped to the
	// gateway's lifetime) at teardown.
	cancel context.CancelFunc

	// peer is the guest's socket address, learned from its first frame:
	// libkrun creates its own socket and connects to ours, so the return
	// address is not known until it speaks. peerReady closes once it is
	// known, so the HOST-initiated direction can wait rather than silently
	// dropping frames into a void.
	peerMu    sync.Mutex
	peer      *net.UnixAddr
	peerReady chan struct{}

	closeOnce sync.Once
	closeErr  error
}

// bindNetGateway binds the backing socket and brings up the virtual network.
// The caller owns the gateway for the VM's lifetime and must Close it.
//
// dial is the host-side transport to an AUTHORIZED destination. It is the
// same egress.DialFunc seam the CONNECT proxy takes, and for the same two
// reasons: the bound external interface stays host-controlled, and dial
// faults become injectable. nil selects egress.HostDial. Substituting it
// changes only the transport — every allow/deny decision still comes from
// the authorizer. Refs: FR-17.7, SEC-04, SEC-10
func bindNetGateway(path string, deps netDeps) (*netGateway, error) {
	deps = deps.withDefaults()
	if deps.auth == nil {
		return nil, fmt.Errorf(
			"%w: refusing to serve guest egress with no authorizer — that would be an open network",
			model.ErrSandboxBackendUnavailable)
	}
	conn, err := bindUnixgram(path, "gateway socket")
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	g := &netGateway{
		conn:      conn,
		path:      path,
		ch:        channel.New(256, guestMTU, gatewayMAC),
		auth:      deps.auth,
		dns:       deps.dns,
		dial:      deps.dial,
		logger:    deps.logger,
		cancel:    cancel,
		peerReady: make(chan struct{}),
	}
	if err := g.buildStack(); err != nil {
		cancel()
		_ = conn.Close()
		_ = os.Remove(path)
		if g.stack != nil {
			g.stack.Destroy() // the processor goroutines start at stack.New
		}
		return nil, err
	}
	if err := g.serveDNS(ctx); err != nil {
		cancel()
		_ = conn.Close()
		_ = os.Remove(path)
		g.stack.Destroy()
		return nil, err
	}
	go g.pumpToGuest()
	go g.pumpFromGuest()
	return g, nil
}

// buildStack brings up the ethernet-framed netstack and installs the
// connection forwarder that enforces policy.
func (g *netGateway) buildStack() error {
	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, arp.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
	})
	g.stack = s // published before any failure so the caller can destroy it
	// libkrun's virtio-net carries real ethernet frames, not bare IP.
	if err := s.CreateNIC(gwNICID, ethernet.New(g.ch)); err != nil {
		return fmt.Errorf("gateway nic: %v", err)
	}
	pa := tcpip.ProtocolAddress{
		Protocol: ipv4.ProtocolNumber,
		AddressWithPrefix: tcpip.AddressWithPrefix{
			Address:   tcpip.AddrFromSlice(net.ParseIP(gatewayIP).To4()),
			PrefixLen: gwPrefixLen,
		},
	}
	if err := s.AddProtocolAddress(gwNICID, pa, stack.AddressProperties{}); err != nil {
		return fmt.Errorf("gateway address: %v", err)
	}
	// A transparent gateway must accept frames addressed to arbitrary
	// destinations and answer as them; that is what lets it terminate the
	// guest's connections instead of routing them.
	_ = s.SetPromiscuousMode(gwNICID, true)
	_ = s.SetSpoofing(gwNICID, true)
	s.SetRouteTable([]tcpip.Route{{Destination: header.IPv4EmptySubnet, NIC: gwNICID}})

	fwd := tcp.NewForwarder(s, 0, gwMaxInFlight, g.handleForward)
	s.SetTransportProtocolHandler(tcp.ProtocolNumber, fwd.HandlePacket)
	return nil
}

// serveDNS binds the resolver at gatewayIP:53 inside the stack and serves the
// guest's queries there.
//
// Binding ONE udp endpoint — and no UDP forwarder — is deliberate: guest
// datagrams to anything else have no listener and are dropped, so UDP cannot
// become an unauthorized egress path (there is no per-flow authorization for
// it). DNS must go through the host resolver because allowlisting by NAME
// depends on the host resolving and PINNING the address. Refs: SEC-04, SEC-07
func (g *netGateway) serveDNS(ctx context.Context) error {
	if g.dns == nil {
		return nil // no resolver wired: guest DNS is dropped like other UDP
	}
	pc, err := gonet.DialUDP(g.stack, &tcpip.FullAddress{
		NIC:  gwNICID,
		Addr: tcpip.AddrFromSlice(net.ParseIP(gatewayIP).To4()),
		Port: dnsPort,
	}, nil, ipv4.ProtocolNumber)
	if err != nil {
		return fmt.Errorf("bind gateway resolver on %s:%d: %w", gatewayIP, dnsPort, err)
	}
	go func() {
		defer func() { _ = pc.Close() }()
		_ = g.dns.ServeUDP(ctx, pc)
	}()
	return nil
}

// handleForward is the enforcement point: netstack hands over each connection
// request BEFORE the handshake completes, so a denied flow is reset and never
// reaches the host network. Refs: SEC-04, FR-17.8
func (g *netGateway) handleForward(r *tcp.ForwarderRequest) {
	id := r.ID()
	dst := net.IP(id.LocalAddress.AsSlice()).String()
	port := int(id.LocalPort)

	decision, err := g.auth.Authorize(context.Background(), egress.Flow{
		Protocol: "tcp", Host: dst, Port: port,
	})
	if err != nil {
		// A policy DENY is an expected outcome the authorizer already
		// audits; an authorizer that could not decide is a fault, and the
		// two need different operator responses even though the guest sees
		// the same reset.
		g.logger.Error("egress authorization failed; flow reset",
			"event", "egress_authorize_failed", "dest_ip", dst, "dest_port", port,
			"error", err.Error())
		r.Complete(true)
		return
	}
	if !decision.Allow {
		r.Complete(true) // RST: the handshake never completes
		return
	}

	// Connect to the PINNED destination the authorizer returned — never a
	// re-resolution (DNS-rebind defense). Refs: SEC-04
	dialCtx, cancel := context.WithTimeout(context.Background(), gwDialTimeout)
	defer cancel()
	outbound, err := g.dial(dialCtx, decision.DestIP, port)
	if err != nil {
		// The flow was ALLOWED and the host side then failed, so the guest
		// sees a reset whose cause is entirely host-side and otherwise
		// invisible to it.
		g.logger.Warn("allowed egress flow could not be dialed; flow reset",
			"event", "egress_dial_failed", "dest_ip", decision.DestIP.String(),
			"dest_port", port, "rule", decision.Rule, "error", err.Error())
		r.Complete(true)
		return
	}

	var wq waiter.Queue
	ep, tcpErr := r.CreateEndpoint(&wq)
	if tcpErr != nil {
		_ = outbound.Close()
		r.Complete(true)
		return
	}
	r.Complete(false)

	// Splice in a goroutine: handleForward must return promptly to release
	// the forwarder's in-flight slot (gwMaxInFlight), and egress.Splice
	// blocks until the flow ends. Reusing it rather than a local io.Copy
	// pair is what closes BOTH sides when either half-closes.
	guestConn := gonet.NewTCPConn(&wq, ep)
	go egress.Splice(guestConn, outbound)
}

// DialGuestPort opens a host->guest connection to a port inside the guest.
//
// This is the INBOUND direction, and it is deliberately asymmetric to the
// outbound one: the host initiates, the guest only accepts, and no guest-side
// code can use it to reach out. It satisfies the dialer the existing SEC-09
// one-way port publisher expects, so publishing a guest port (an sshd, a dev
// server) reuses that machinery unchanged. Refs: SEC-09, FR-17.8
func (g *netGateway) DialGuestPort(ctx context.Context, port int) (net.Conn, error) {
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("%w: guest port %d out of range",
			model.ErrSandboxBackendUnavailable, port)
	}
	// The guest's return address is only known once it has transmitted, and
	// until then outbound frames have nowhere to go. A booted guest ARPs the
	// gateway almost immediately, so this is a startup race, not a wait.
	select {
	case <-g.peerReady:
	case <-ctx.Done():
		return nil, fmt.Errorf("%w: guest network is not up yet (no frame received from it): %w",
			model.ErrSandboxBackendUnavailable, ctx.Err())
	}
	return gonet.DialContextTCP(ctx, g.stack, tcpip.FullAddress{
		NIC:  gwNICID,
		Addr: tcpip.AddrFromSlice(net.ParseIP(guestIP).To4()),
		Port: uint16(port), //nolint:gosec // OK: range-checked immediately above
	}, ipv4.ProtocolNumber)
}

// pumpFromGuest injects the guest's frames into the stack, learning the
// guest's socket address from the first one.
func (g *netGateway) pumpFromGuest() {
	buf := make([]byte, maxFrameBytes)
	for {
		n, from, err := g.conn.ReadFromUnix(buf)
		if err != nil {
			return // socket closed at teardown
		}
		if from != nil && from.Name != "" {
			g.peerMu.Lock()
			if g.peer == nil {
				g.peer = from
				close(g.peerReady)
			}
			g.peerMu.Unlock()
		}
		// MakeWithData copies into a pooled chunk, so buf can be reused; the
		// packet is released after injection to return it to gvisor's pool.
		pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
			Payload: buffer.MakeWithData(buf[:n]),
		})
		g.ch.InjectInbound(header.IPv4ProtocolNumber, pkt)
		pkt.DecRef()
	}
}

// pumpToGuest writes the stack's frames back to the guest's socket. Frames
// produced before the guest has spoken are dropped: there is nowhere to send
// them, and the guest always speaks first (ARP).
func (g *netGateway) pumpToGuest() {
	for {
		pkt := g.ch.ReadContext(context.Background())
		if pkt == nil {
			return
		}
		if to := g.guestAddr(); to != nil {
			_, _ = g.conn.WriteToUnix(pkt.ToView().AsSlice(), to)
		}
		pkt.DecRef()
	}
}

func (g *netGateway) guestAddr() *net.UnixAddr {
	g.peerMu.Lock()
	defer g.peerMu.Unlock()
	return g.peer
}

// Close tears the gateway down and removes the socket, leaving no residue
// (SEC-10). It is idempotent; later calls report the first call's result.
//
// Destroying the stack is not optional bookkeeping: netstack starts a TCP
// processor goroutine per CPU at construction, and Destroy is also what
// ABORTS still-established endpoints — without it a torn-down sandbox's
// spliced connections stay open to the host after its VM is gone.
func (g *netGateway) Close() error {
	g.closeOnce.Do(func() {
		g.cancel()
		g.closeErr = g.conn.Close()
		g.stack.Destroy()
		g.ch.Close()
		if err := os.Remove(g.path); err != nil && !os.IsNotExist(err) && g.closeErr == nil {
			g.closeErr = fmt.Errorf("remove gateway socket %s: %w", g.path, err)
		}
	})
	return g.closeErr
}
