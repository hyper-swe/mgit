package libkrun

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
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
	conn  *net.UnixConn
	path  string
	ch    *channel.Endpoint
	stack *stack.Stack
	auth  flowAuthorizer
	dns   dnsResolver

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
// Refs: FR-17.7, SEC-10
func bindNetGateway(path string, auth flowAuthorizer, dns dnsResolver) (*netGateway, error) {
	if auth == nil {
		return nil, fmt.Errorf(
			"%w: refusing to serve guest egress with no authorizer — that would be an open network",
			model.ErrSandboxBackendUnavailable)
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("clear stale gateway socket %s: %w", path, err)
	}
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		return nil, fmt.Errorf("bind gateway socket %s: %w", path, err)
	}
	if err := os.Chmod(path, denySocketMode); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("restrict gateway socket %s: %w", path, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	g := &netGateway{
		conn:      conn,
		path:      path,
		ch:        channel.New(256, guestMTU, gatewayMAC),
		auth:      auth,
		dns:       dns,
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
	if err != nil || !decision.Allow {
		r.Complete(true) // RST: the handshake never completes
		return
	}

	// Connect to the PINNED destination the authorizer returned — never a
	// re-resolution (DNS-rebind defense). Refs: SEC-04
	dialCtx, cancel := context.WithTimeout(context.Background(), gwDialTimeout)
	defer cancel()
	outbound, err := (&net.Dialer{}).DialContext(dialCtx, "tcp",
		net.JoinHostPort(decision.DestIP.String(), strconv.Itoa(port)))
	if err != nil {
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

	guestConn := gonet.NewTCPConn(&wq, ep)
	go func() { _, _ = io.Copy(outbound, guestConn); _ = outbound.Close() }()
	go func() { _, _ = io.Copy(guestConn, outbound); _ = guestConn.Close() }()
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
