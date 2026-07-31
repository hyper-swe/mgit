package libkrun

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

	"github.com/hyper-swe/mgit/internal/sandboxd/egress"
)

// The allowlist/open host backend: a userspace TCP/IP stack that terminates
// the guest's connections and admits ONLY what the authorizer allows. These
// tests drive it over a REAL unixgram socket with a real guest-side netstack,
// so they exercise the same wire format libkrun uses — but need no VM, no
// root, and no iptables. That is the point: egress enforcement becomes
// unit-testable. Refs: FR-17.7, FR-17.8, SEC-04, ADR-010

// stubAuthorizer admits exactly one destination, pinned to a rewritten host.
type stubAuthorizer struct{ allowed map[string]string }

func (s *stubAuthorizer) Authorize(_ context.Context, f egress.Flow) (egress.Decision, error) {
	if pinned, ok := s.allowed[f.Host]; ok {
		return egress.Decision{Allow: true, DestIP: netip.MustParseAddr(pinned), Rule: "allowlist"}, nil
	}
	return egress.Decision{Rule: "default-deny"}, nil
}

// echoServer stands in for a real internet host.
func echoServer(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(c, c); _ = c.Close() }()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

// fakeGuest is a netstack on the other end of the gateway's socket, standing
// in for the libkrun VM's virtio-net device.
func fakeGuest(t *testing.T, dir, gwPath string) *stack.Stack {
	t.Helper()
	addr := &net.UnixAddr{Name: filepath.Join(dir, "guest.sock"), Net: "unixgram"}
	conn, err := net.ListenUnixgram("unixgram", addr)
	if err != nil {
		t.Fatalf("bind guest socket: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	ch := channel.New(256, guestMTU, tcpip.LinkAddress("\x02\x00\x00\x00\x00\x01"))
	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, arp.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
	})
	if err := s.CreateNIC(gwNICID, ethernet.New(ch)); err != nil {
		t.Fatalf("guest nic: %v", err)
	}
	pa := tcpip.ProtocolAddress{
		Protocol: ipv4.ProtocolNumber,
		AddressWithPrefix: tcpip.AddressWithPrefix{
			Address:   tcpip.AddrFromSlice(net.ParseIP(guestIP).To4()),
			PrefixLen: gwPrefixLen,
		},
	}
	if err := s.AddProtocolAddress(gwNICID, pa, stack.AddressProperties{}); err != nil {
		t.Fatalf("guest addr: %v", err)
	}
	s.SetRouteTable([]tcpip.Route{{Destination: header.IPv4EmptySubnet, NIC: gwNICID}})
	t.Cleanup(s.Destroy) // netstack starts per-CPU goroutines at construction

	peer := &net.UnixAddr{Name: gwPath, Net: "unixgram"}
	go func() {
		for {
			pkt := ch.ReadContext(context.Background())
			if pkt == nil {
				return
			}
			_, _ = conn.WriteToUnix(pkt.ToView().AsSlice(), peer)
			pkt.DecRef()
		}
	}()
	go func() {
		buf := make([]byte, 65536)
		for {
			n, _, err := conn.ReadFromUnix(buf)
			if err != nil {
				return
			}
			frame := make([]byte, n)
			copy(frame, buf[:n])
			ch.InjectInbound(header.IPv4ProtocolNumber, stack.NewPacketBuffer(stack.PacketBufferOptions{
				Payload: buffer.MakeWithData(frame),
			}))
		}
	}()
	return s
}

// guestDialUDP opens a UDP socket from the simulated guest.
func guestDialUDP(t *testing.T, s *stack.Stack, ip string, port int) (net.Conn, error) {
	t.Helper()
	return gonet.DialUDP(s, nil, &tcpip.FullAddress{
		NIC:  gwNICID,
		Addr: tcpip.AddrFromSlice(net.ParseIP(ip).To4()),
		Port: uint16(port), //nolint:gosec // OK: test constant
	}, ipv4.ProtocolNumber)
}

// guestDial performs a TCP connect from the simulated guest.
func guestDial(t *testing.T, s *stack.Stack, ip string, port int) (net.Conn, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	return gonet.DialContextTCP(ctx, s, tcpip.FullAddress{
		NIC:  gwNICID,
		Addr: tcpip.AddrFromSlice(net.ParseIP(ip).To4()),
		Port: uint16(port), //nolint:gosec // OK: test port from a listener
	}, ipv4.ProtocolNumber)
}

func TestNetGateway_EnforcesTheAuthorizerPerConnection(t *testing.T) {
	port := echoServer(t)
	dir := shortTempDir(t)
	gwPath := filepath.Join(dir, proxySocketName)

	gw, err := bindNetGateway(gwPath, netDeps{auth: &stubAuthorizer{
		allowed: map[string]string{"93.184.216.34": "127.0.0.1"},
	}})
	if err != nil {
		t.Fatalf("bindNetGateway: %v", err)
	}
	t.Cleanup(func() { _ = gw.Close() })

	guest := fakeGuest(t, dir, gwPath)

	tests := []struct {
		name      string
		dstIP     string
		wantAllow bool
	}{
		{name: "allowlisted_is_spliced_to_the_host", dstIP: "93.184.216.34", wantAllow: true},
		{name: "off_allowlist_is_reset", dstIP: "1.2.3.4"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn, err := guestDial(t, guest, tt.dstIP, port)
			if !tt.wantAllow {
				if err == nil {
					_ = conn.Close()
					t.Fatal("an off-allowlist destination was reachable — egress leak")
				}
				return
			}
			if err != nil {
				t.Fatalf("allowlisted destination refused: %v", err)
			}
			defer func() { _ = conn.Close() }()

			// Prove real bytes traverse the gateway, not just a handshake.
			if _, err := conn.Write([]byte("ping")); err != nil {
				t.Fatalf("write: %v", err)
			}
			_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			buf := make([]byte, 4)
			if _, err := io.ReadFull(conn, buf); err != nil {
				t.Fatalf("read echo: %v", err)
			}
			if string(buf) != "ping" {
				t.Errorf("echo = %q, want %q", buf, "ping")
			}
		})
	}
}

func TestNetGateway_Close_RemovesTheSocketAndIsIdempotent(t *testing.T) {
	dir := shortTempDir(t)
	path := filepath.Join(dir, proxySocketName)

	gw, err := bindNetGateway(path, netDeps{auth: &stubAuthorizer{}})
	if err != nil {
		t.Fatalf("bindNetGateway: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("gateway socket still present after Close (SEC-10 residue)")
	}
	if err := gw.Close(); err != nil {
		t.Errorf("second Close should be a no-op, got %v", err)
	}
}

func TestBindHostPeer_AllowlistAndOpen_NowGetAGateway(t *testing.T) {
	for _, mode := range []string{"allowlist", "open"} {
		t.Run(mode, func(t *testing.T) {
			backing, err := netBackingFor("sbx-abc123", mode, shortTempDir(t))
			if err != nil {
				t.Fatalf("netBackingFor: %v", err)
			}
			peer, err := bindHostPeer(backing, netDeps{auth: &stubAuthorizer{}})
			if err != nil {
				t.Fatalf("mode %q must now be servable, got %v", mode, err)
			}
			_ = peer.Close()
		})
	}
}

func TestBindNetGateway_FailsClosed(t *testing.T) {
	tests := []struct {
		name string
		path func(t *testing.T) string
		auth flowAuthorizer
		want string
	}{
		{
			// A gateway with no policy would be an open network — worse than
			// no gateway at all.
			name: "no_authorizer",
			path: func(t *testing.T) string { return filepath.Join(shortTempDir(t), proxySocketName) },
			auth: nil,
			want: "no authorizer",
		},
		{
			name: "unbindable_path",
			path: func(t *testing.T) string { return filepath.Join(shortTempDir(t), "nope", proxySocketName) },
			auth: &stubAuthorizer{},
			want: "bind gateway socket",
		},
		{
			name: "stale_path_is_a_nonempty_dir",
			path: func(t *testing.T) string {
				p := filepath.Join(shortTempDir(t), proxySocketName)
				if err := os.MkdirAll(filepath.Join(p, "child"), 0o700); err != nil {
					t.Fatalf("seed: %v", err)
				}
				return p
			},
			auth: &stubAuthorizer{},
			want: "clear stale gateway socket",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gw, err := bindNetGateway(tt.path(t), netDeps{auth: tt.auth})
			if err == nil {
				_ = gw.Close()
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not mention %q", err, tt.want)
			}
		})
	}
}

// An authorizer that errors must be treated as a denial, never as an allow.
func TestNetGateway_AuthorizerError_IsTreatedAsDenial(t *testing.T) {
	port := echoServer(t)
	dir := shortTempDir(t)
	gwPath := filepath.Join(dir, proxySocketName)

	gw, err := bindNetGateway(gwPath, netDeps{auth: errAuthorizer{}})
	if err != nil {
		t.Fatalf("bindNetGateway: %v", err)
	}
	t.Cleanup(func() { _ = gw.Close() })

	guest := fakeGuest(t, dir, gwPath)
	if conn, err := guestDial(t, guest, "93.184.216.34", port); err == nil {
		_ = conn.Close()
		t.Fatal("a flow was admitted while the authorizer was failing — fail-open")
	}
}

type errAuthorizer struct{}

func (errAuthorizer) Authorize(context.Context, egress.Flow) (egress.Decision, error) {
	return egress.Decision{Allow: true}, errAuthFailed // Allow set: must still be denied
}

var errAuthFailed = errors.New("authorizer unavailable")

// The allowlisted destination is admitted but the host dial fails: the guest
// must be reset, not left hanging.
func TestNetGateway_HostDialFails_ResetsTheGuest(t *testing.T) {
	dir := shortTempDir(t)
	gwPath := filepath.Join(dir, proxySocketName)

	// Pin to a port nothing listens on.
	gw, err := bindNetGateway(gwPath, netDeps{auth: &stubAuthorizer{allowed: map[string]string{"93.184.216.34": "127.0.0.1"}}})
	if err != nil {
		t.Fatalf("bindNetGateway: %v", err)
	}
	t.Cleanup(func() { _ = gw.Close() })

	guest := fakeGuest(t, dir, gwPath)
	if conn, err := guestDial(t, guest, "93.184.216.34", 1); err == nil {
		_ = conn.Close()
		t.Fatal("guest connection completed although the host dial could not")
	}
}

// stubDNS answers every query with a fixed payload, proving the DNS seam is
// wired to the guest. *egress.DNSServer satisfies the same interface, so
// swapping in the real (SEC-07 pinning) server is a substitution.
type stubDNS struct{ reply []byte }

func (s stubDNS) ServeUDP(ctx context.Context, pc net.PacketConn) error {
	buf := make([]byte, 1500)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			return err
		}
		_ = n
		if _, err := pc.WriteTo(s.reply, from); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
	}
}

// Guest DNS must reach the host-side resolver, because allowlisting by NAME
// depends on the host resolving and pinning the address (SEC-04/SEC-07).
func TestNetGateway_GuestDNS_ReachesTheHostResolver(t *testing.T) {
	dir := shortTempDir(t)
	gwPath := filepath.Join(dir, proxySocketName)

	gw, err := bindNetGateway(gwPath, netDeps{auth: &stubAuthorizer{}, dns: stubDNS{reply: []byte("dns-answer")}})
	if err != nil {
		t.Fatalf("bindNetGateway: %v", err)
	}
	t.Cleanup(func() { _ = gw.Close() })

	guest := fakeGuest(t, dir, gwPath)
	conn, err := guestDialUDP(t, guest, gatewayIP, dnsPort)
	if err != nil {
		t.Fatalf("guest could not reach the gateway resolver: %v", err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.Write([]byte("query")); err != nil {
		t.Fatalf("write query: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("no DNS answer returned: %v", err)
	}
	if got := string(buf[:n]); got != "dns-answer" {
		t.Errorf("answer = %q, want %q", got, "dns-answer")
	}
}

// Everything except the gateway resolver stays dropped: UDP has no
// authorization path, so a guest must not be able to tunnel out over it.
func TestNetGateway_GuestUDP_ToAnythingElse_IsDropped(t *testing.T) {
	dir := shortTempDir(t)
	gwPath := filepath.Join(dir, proxySocketName)

	gw, err := bindNetGateway(gwPath, netDeps{auth: &stubAuthorizer{}, dns: stubDNS{reply: []byte("x")}})
	if err != nil {
		t.Fatalf("bindNetGateway: %v", err)
	}
	t.Cleanup(func() { _ = gw.Close() })

	guest := fakeGuest(t, dir, gwPath)
	conn, err := guestDialUDP(t, guest, "1.2.3.4", 5353)
	if err != nil {
		return // refused outright is also fine
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write([]byte("exfil")); err != nil {
		return
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if n, err := conn.Read(make([]byte, 64)); err == nil {
		t.Fatalf("guest UDP to an unauthorized destination got %d bytes back — egress path", n)
	}
}
