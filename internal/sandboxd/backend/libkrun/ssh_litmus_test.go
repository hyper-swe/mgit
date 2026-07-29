package libkrun

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/egress"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

// LITMUS TEST — the sandbox network is only credible if it gets all three of
// these right at once, with a REAL protocol rather than a byte echo:
//
//  1. the guest can run an SSH server and the HOST can connect to it
//     (inbound works — the sandbox is usable, SEC-09 one-way publish);
//  2. the guest can NOT open a reverse tunnel to an arbitrary relay
//     (outbound is default-deny — the containment claim, T3 exfiltration);
//  3. the same reverse tunnel DOES work once policy allows the relay
//     (the control is policy, not an accident of plumbing).
//
// Refs: FR-17.7, FR-17.8, SEC-04, SEC-09, ADR-010

// sshHostKey generates a throwaway host key.
func sshHostKey(t *testing.T) ssh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate host key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	return signer
}

// runGuestSSHD serves a minimal SSH server on the guest's stack at :22 that
// answers "exec" with a fixed banner.
func runGuestSSHD(t *testing.T, guest *stack.Stack) {
	t.Helper()
	cfg := &ssh.ServerConfig{NoClientAuth: true}
	cfg.AddHostKey(sshHostKey(t))

	ln, err := gonet.ListenTCP(guest, tcpip.FullAddress{
		NIC:  gwNICID,
		Addr: tcpip.AddrFromSlice(net.ParseIP(guestIP).To4()),
		Port: 22,
	}, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatalf("guest sshd listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go serveSSHConn(c, cfg)
		}
	}()
}

func serveSSHConn(c net.Conn, cfg *ssh.ServerConfig) {
	sc, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		_ = c.Close()
		return
	}
	defer func() { _ = sc.Close() }()
	go ssh.DiscardRequests(reqs)
	for newCh := range chans {
		if newCh.ChannelType() != "session" {
			_ = newCh.Reject(ssh.UnknownChannelType, "only sessions")
			continue
		}
		ch, chReqs, err := newCh.Accept()
		if err != nil {
			return
		}
		go func() {
			for req := range chReqs {
				if req.Type == "exec" {
					_ = req.Reply(true, nil)
					_, _ = io.WriteString(ch, "hello-from-guest")
					_, _ = ch.SendRequest("exit-status", false, []byte{0, 0, 0, 0})
					_ = ch.Close()
				} else if req.WantReply {
					_ = req.Reply(false, nil)
				}
			}
		}()
	}
}

// TestLitmus_HostCanSSHIntoTheGuest proves the sandbox is usable inbound: a
// real SSH handshake and command, host -> guest, through the gateway.
func TestLitmus_HostCanSSHIntoTheGuest(t *testing.T) {
	dir := shortTempDir(t)
	gwPath := filepath.Join(dir, proxySocketName)
	gw, err := bindNetGateway(gwPath, netDeps{auth: &stubAuthorizer{}})
	if err != nil {
		t.Fatalf("bindNetGateway: %v", err)
	}
	t.Cleanup(func() { _ = gw.Close() })

	guest := fakeGuest(t, dir, gwPath)
	runGuestSSHD(t, guest)

	// A booted guest has already put frames on the wire (ARP/route setup)
	// before anything dials in; reproduce that so the gateway knows the
	// guest's return address.
	if c, err := guestDial(t, guest, gatewayIP, 9); err == nil {
		_ = c.Close()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// The HOST initiates — the one-way direction the port publisher uses.
	raw, err := gw.DialGuestPort(ctx, 22)
	if err != nil {
		t.Fatalf("host could not reach the guest sshd: %v", err)
	}
	defer func() { _ = raw.Close() }()

	_ = raw.SetDeadline(time.Now().Add(10 * time.Second))
	cc, chans, reqs, err := ssh.NewClientConn(raw, "guest:22", &ssh.ClientConfig{
		User:            "agent",
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec // OK: throwaway key in-test
		Timeout:         10 * time.Second,
	})
	if err != nil {
		t.Fatalf("SSH handshake to the guest failed: %v", err)
	}
	client := ssh.NewClient(cc, chans, reqs)
	defer func() { _ = client.Close() }()

	sess, err := client.NewSession()
	if err != nil {
		t.Fatalf("ssh session: %v", err)
	}
	defer func() { _ = sess.Close() }()

	out, err := sess.Output("whoami")
	if err != nil {
		t.Fatalf("ssh exec: %v", err)
	}
	if string(out) != "hello-from-guest" {
		t.Errorf("ssh output = %q, want %q", out, "hello-from-guest")
	}
	t.Logf("LITMUS 1 PASS: host completed a real SSH session into the microVM guest")
}

// TestLitmus_GuestReverseTunnel_IsGovernedByPolicy proves the outbound half:
// a guest trying to phone home to a relay (the shape of `ssh -R`, and of
// exfiltration generally) is blocked by default and permitted only when the
// allowlist names the relay.
func TestLitmus_GuestReverseTunnel_IsGovernedByPolicy(t *testing.T) {
	relayPort := echoServer(t) // stands in for an attacker-controlled relay
	const relayIP = "203.0.113.9"

	tests := []struct {
		name      string
		allowed   map[string]string
		wantTunel bool
	}{
		{
			// The containment claim: an agent (or a poisoned dependency)
			// cannot open a tunnel out of the sandbox.
			name:    "default_deny_blocks_the_reverse_tunnel",
			allowed: map[string]string{},
		},
		{
			// The same tunnel must succeed once policy permits it, or the
			// block above would just mean "networking is broken".
			name:      "allowlisting_the_relay_permits_it",
			allowed:   map[string]string{relayIP: "127.0.0.1"},
			wantTunel: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := shortTempDir(t)
			gwPath := filepath.Join(dir, proxySocketName)
			gw, err := bindNetGateway(gwPath, netDeps{auth: &stubAuthorizer{allowed: tt.allowed}})
			if err != nil {
				t.Fatalf("bindNetGateway: %v", err)
			}
			t.Cleanup(func() { _ = gw.Close() })

			guest := fakeGuest(t, dir, gwPath)

			// The guest opens the outbound leg a reverse tunnel needs.
			conn, err := guestDial(t, guest, relayIP, relayPort)
			if !tt.wantTunel {
				if err == nil {
					_ = conn.Close()
					t.Fatal("the guest established a tunnel to an un-allowlisted relay — " +
						"containment failure (T3 exfiltration)")
				}
				t.Logf("LITMUS 2 PASS: reverse tunnel blocked by default-deny (%v)", err)
				return
			}
			if err != nil {
				t.Fatalf("allowlisted relay unreachable: %v", err)
			}
			defer func() { _ = conn.Close() }()

			// Carry payload, so this is a usable tunnel and not just a SYN.
			if _, err := conn.Write([]byte("tunneled")); err != nil {
				t.Fatalf("tunnel write: %v", err)
			}
			_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			buf := make([]byte, 8)
			if _, err := io.ReadFull(conn, buf); err != nil {
				t.Fatalf("tunnel read: %v", err)
			}
			if string(buf) != "tunneled" {
				t.Errorf("tunnel payload = %q, want %q", buf, "tunneled")
			}
			t.Logf("LITMUS 3 PASS: the same tunnel works once policy allows the relay")
		})
	}
}

// LITMUS LEG 3, against the REAL egress.Authorizer rather than a stub.
//
// The difficulty this solves: for an ALLOW to be observable the host must
// actually dial the pinned destination, and it cannot be pointed at a local
// service by policy — SEC-04 unconditionally denies loopback and private
// ranges (T9), so an allowlist entry naming 127.0.0.1 is refused before the
// allowlist is even consulted. Relaxing that to make a test pass would be
// deleting the control being tested.
//
// So policy stays completely unmodified — a PUBLIC address is allowlisted and
// the authorizer pins it — and only the TRANSPORT is redirected, through the
// egress.DialFunc seam the CONNECT proxy already uses. The authorizer still
// decides; the dialer only decides where the approved bytes physically go.
// Refs: SEC-04, FR-17.8, MGIT-61.10

// redirectDial returns an egress.DialFunc that sends every AUTHORIZED
// connection to a local listener, recording the destination the authorizer
// pinned so the test can assert policy saw the real address.
func redirectDial(t *testing.T, toPort int) (egress.DialFunc, func() []netip.Addr) {
	t.Helper()
	var mu sync.Mutex
	var pinned []netip.Addr
	return func(ctx context.Context, ip netip.Addr, _ int) (net.Conn, error) {
			mu.Lock()
			pinned = append(pinned, ip)
			mu.Unlock()
			var d net.Dialer
			return d.DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(toPort)))
		}, func() []netip.Addr {
			mu.Lock()
			defer mu.Unlock()
			return append([]netip.Addr(nil), pinned...)
		}
}

// realAuthorizer builds the production egress stack for one allowlist policy.
func realAuthorizer(t *testing.T, allowlist []string) *egress.Authorizer {
	t.Helper()
	sup, err := egress.NewSupervisor(egress.SupervisorConfig{
		SandboxID: "sbx-litmus3", TaskID: "MGIT-61.10",
		Policy: model.NetworkPolicy{Mode: model.NetworkModeAllowlist, Allowlist: allowlist},
		Audit:  logAuditor{logger: slog.New(slog.NewTextHandler(io.Discard, nil))},
		Lookup: egress.SystemLookup(nil),
		Dial:   egress.HostDial,
		Clock:  func() time.Time { return time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC) },
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("egress supervisor: %v", err)
	}
	return sup.Authorizer()
}

func TestLitmus3_RealAuthorizer_PermitsTheTunnelOnlyWhenPolicyDoes(t *testing.T) {
	relayPort := echoServer(t)
	// A PUBLIC address: a reserved/documentation range would be refused by
	// the unconditional IP denials before the allowlist is consulted, and the
	// test would pass without exercising policy at all.
	const relayIP = "93.184.216.34"

	tests := []struct {
		name      string
		allowlist []string
		wantOpen  bool
	}{
		{
			// The containment claim, now through the production authorizer.
			name:      "default_deny_blocks_the_tunnel",
			allowlist: []string{"proxy.golang.org:443"},
		},
		{
			// The same tunnel must work once policy names the relay, or the
			// deny above would only mean "networking is broken".
			name:      "allowlisting_the_relay_permits_it",
			allowlist: []string{relayIP + ":" + strconv.Itoa(relayPort)},
			wantOpen:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := shortTempDir(t)
			gwPath := filepath.Join(dir, proxySocketName)
			dial, pinnedAddrs := redirectDial(t, relayPort)

			gw, err := bindNetGateway(gwPath, netDeps{auth: realAuthorizer(t, tt.allowlist), dial: dial})
			if err != nil {
				t.Fatalf("bindNetGateway: %v", err)
			}
			t.Cleanup(func() { _ = gw.Close() })

			guest := fakeGuest(t, dir, gwPath)
			conn, err := guestDial(t, guest, relayIP, relayPort)

			if !tt.wantOpen {
				if err == nil {
					_ = conn.Close()
					t.Fatal("the guest tunneled to a relay policy does not name (T3 exfiltration)")
				}
				if len(pinnedAddrs()) != 0 {
					t.Error("a denied flow reached the dialer; the authorizer must stop it first")
				}
				t.Logf("LITMUS 3a PASS (real authorizer): tunnel denied by policy (%v)", err)
				return
			}

			if err != nil {
				t.Fatalf("allowlisted relay unreachable: %v", err)
			}
			defer func() { _ = conn.Close() }()

			if _, err := conn.Write([]byte("tunneled")); err != nil {
				t.Fatalf("tunnel write: %v", err)
			}
			_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			buf := make([]byte, 8)
			if _, err := io.ReadFull(conn, buf); err != nil {
				t.Fatalf("tunnel read: %v", err)
			}
			if string(buf) != "tunneled" {
				t.Errorf("tunnel payload = %q, want %q", buf, "tunneled")
			}
			// Policy must have seen and pinned the PUBLIC address; only the
			// transport was redirected.
			got := pinnedAddrs()
			if len(got) == 0 || got[0].String() != relayIP {
				t.Errorf("authorizer pinned %v, want the public address %s — if this is "+
					"127.0.0.1 the test redirected POLICY, not just transport", got, relayIP)
			}
			t.Logf("LITMUS 3b PASS (real authorizer): policy pinned %s and the tunnel carried payload", relayIP)
		})
	}
}
