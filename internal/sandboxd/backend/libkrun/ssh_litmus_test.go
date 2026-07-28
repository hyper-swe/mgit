package libkrun

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
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
	gw, err := bindNetGateway(gwPath, &stubAuthorizer{}, nil)
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
			gw, err := bindNetGateway(gwPath, &stubAuthorizer{allowed: tt.allowed}, nil)
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
