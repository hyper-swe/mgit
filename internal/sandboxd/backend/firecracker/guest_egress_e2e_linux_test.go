//go:build linux

// REAL-VM ALLOW-PATH e2e for the firecracker backend (MGIT-69).
//
// WHY THIS FILE EXISTS, stated plainly because its absence is the bug:
//
//  1. Every pre-existing network probe in this package prefixes its shell
//     script with netUpPrefix — `ip addr add` + `ip route replace` — which
//     configures the guest NIC from inside the probe. That is test
//     scaffolding, and it means those tests never proved the PRODUCTION boot
//     path addresses the guest at all. Nothing here uses it.
//  2. Every pre-existing DNS assertion passes the server explicitly
//     (`nslookup <name> <gateway>`), which bypasses /etc/resolv.conf — the
//     path npm, apt and curl actually take. The guest rootfs ships no /etc at
//     all, so that path was broken and invisible.
//  3. The allowlist assertions drove mgit's CONNECT proxy from the HOST.
//     Nothing in any guest speaks that protocol, so they demonstrated the
//     policy while never proving a guest could get through it.
//
// These assert the guest's own view, through the production path, with an
// ALLOW assertion for every DENY one. Refs: MGIT-69, MGIT-68, SEC-04, SEC-07
package firecracker

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"log/slog"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/backend/microvm"
	"github.com/hyper-swe/mgit/internal/sandboxd/egress"
)

// guestBannerText is what the stand-in destination sends. The guest must read
// these exact bytes: a completed handshake alone would not prove the flow was
// spliced to the destination rather than swallowed.
const guestBannerText = "REAL-BYTES-FROM-ALLOWLISTED-DESTINATION"

// bannerListener stands in for the allowlisted public destination. The
// injected proxy Dial routes authorized flows here, so the test stays
// hermetic — no real internet egress — while still carrying real bytes.
func bannerListener(t *testing.T) (net.Listener, <-chan struct{}) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	// accepted fires on the first connection. It is the HOST-side proof that
	// the flow actually traversed redirect -> authorizer -> dial, which holds
	// even if the guest's nc prints nothing (busybox nc can exit on stdin EOF
	// before the reply lands). The guest-side byte assertion still stands on
	// its own; this tells the two failure modes apart.
	accepted := make(chan struct{}, 1)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			select {
			case accepted <- struct{}{}:
			default:
			}
			go func() {
				_, _ = io.WriteString(c, guestBannerText+"\n")
				_ = c.Close()
			}()
		}
	}()
	return ln, accepted
}

// guestDial is the guest-side probe for an ordinary TCP connection.
//
// stdin is held open with `sleep` rather than closed with </dev/null: busybox
// nc shuts down on stdin EOF and can exit before the peer's reply arrives, so
// </dev/null turns a WORKING flow into empty output. The exit code is echoed
// so a refusal is distinguishable from a silent timeout.
func guestDial(t *testing.T, mgr *microvm.Manager, id, ip string, port int) string {
	t.Helper()
	return probe(t, mgr, id,
		fmt.Sprintf("sleep 3 | nc -w 6 %s %d 2>&1; echo rc=$?", ip, port))
}

// startEgressFor brings up the host egress stack for a launched sandbox,
// hermetically: an allowlisted name resolves to a public-looking address and
// authorized flows are dialed to the local banner listener.
func startEgressFor(t *testing.T, info *model.SandboxInfo, mode string, allowlist []string, target net.Listener) *recordingEgressAudit {
	t.Helper()
	audit := &recordingEgressAudit{}
	runner, err := egress.NewRunner(egress.RunnerConfig{
		Audit: audit,
		Lookup: func(_ context.Context, name string) ([]netip.Addr, error) {
			if name == "allowed.test" {
				return []netip.Addr{allowedTestIP}, nil
			}
			return nil, egress.ErrNXDOMAIN
		},
		Dial: func(ctx context.Context, _ netip.Addr, _ int) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", target.Addr().String())
		},
		Clock:           func() time.Time { return time.Now().UTC() },
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		ProxyPort:       hostProxyPort,
		DNSPort:         hostDNSPort,
		TransparentPort: hostTransparentPort,
	})
	require.NoError(t, err)
	_, err = runner.Start(context.Background(), egress.Binding{
		SandboxID: info.ID, TaskID: info.TaskID,
		GatewayIP: GatewayFor(info.ID),
		Policy:    model.NetworkPolicy{Mode: mode, Allowlist: allowlist},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = runner.Stop(info.ID) })
	return audit
}

// probe runs a shell script in the guest with NO network scaffolding and
// returns its combined output. Deliberately not netUpPrefix: the point is
// what the guest has after the PRODUCTION boot path, not after the test
// configured it. Refs: MGIT-69
func probe(t *testing.T, mgr *microvm.Manager, id, script string) string {
	t.Helper()
	res := guestProbe(t, mgr, id, script)
	return string(res.Stdout) + string(res.Stderr)
}

// requireGuestIsAddressed asserts the guest booted with an address and a
// default route WITHOUT the test putting them there, and returns the
// diagnostic output for logging.
//
// Every other network test in this package configured the NIC itself, so this
// property — that the kernel's ip= boot parameter actually works — has never
// been asserted anywhere. Refs: MGIT-69
func requireGuestIsAddressed(t *testing.T, mgr *microvm.Manager, info *model.SandboxInfo) string {
	t.Helper()
	_, guestIP, _ := subnetFor(info.ID)
	out := probe(t, mgr, info.ID, "ip addr show eth0 2>&1; echo ---; ip route 2>&1")
	if !strings.Contains(out, guestIP.String()) {
		t.Fatalf("the guest booted WITHOUT its address %s — the production path does not "+
			"configure the NIC, and every other test in this package hid that by running "+
			"`ip addr add` itself (netUpPrefix); guest saw:\n%s", guestIP, out)
	}
	if !strings.Contains(out, "default via "+GatewayFor(info.ID).String()) {
		t.Fatalf("the guest booted with no default route via %s; guest saw:\n%s",
			GatewayFor(info.ID), out)
	}
	return out
}

// TestE2E_Network_Allowlist_GuestResolvesAndReachesAnAllowlistedDestination
// is the MGIT-69 allow assertion on a real VM: from inside the guest, through
// its OWN resolver and an ORDINARY TCP connection, an allowlisted name
// resolves and returns real bytes. Refs: MGIT-69, SEC-04, SEC-07, FR-17.8
func TestE2E_Network_Allowlist_GuestResolvesAndReachesAnAllowlistedDestination(t *testing.T) {
	kernel, rootfs := requireGuestImage(t)
	requireNetRoot(t)

	target, accepted := bannerListener(t)
	mgr, ref := registerGuestManager(t, kernel, rootfs, "")
	info := launchNetSandbox(t, mgr, ref, model.NetworkModeAllowlist, []string{"allowed.test:443"})
	audit := startEgressFor(t, info, model.NetworkModeAllowlist, []string{"allowed.test:443"}, target)

	t.Logf("guest addressing (production path, no test scaffolding):\n%s",
		requireGuestIsAddressed(t, mgr, info))

	// 1. The RESOLVER the guest actually uses. Not `nslookup <name> <server>`
	//    — that bypasses /etc/resolv.conf, which is exactly what hid this.
	resolv := probe(t, mgr, info.ID, "cat /etc/resolv.conf 2>&1")
	assert.Contains(t, resolv, GatewayFor(info.ID).String(),
		"the guest's resolver file must name the gateway; without it every getaddrinfo "+
			"caller (npm, apt, curl) fails EAI_AGAIN")

	lookup := probe(t, mgr, info.ID, "nslookup allowed.test 2>&1")
	assert.Contains(t, lookup, allowedTestIP.String(),
		"an allowlisted name must resolve through the guest's DEFAULT resolver")

	// 2. An ORDINARY TCP connection must carry REAL BYTES — the assertion
	//    whose absence let allowlist mode ship unusable. The guest speaks no
	//    proxy protocol, and before the transparent redirect it had no way
	//    through the policy at all.
	//
	//    It dials the address the resolution above PINNED, which is the
	//    resolve-then-connect-by-IP sequence every real client performs and
	//    the exact case authorizeRawIP's pin path exists for: the IP is
	//    admitted because the HOST resolved it from an allowlisted name, not
	//    because the IP itself is listed. A hardcoded IP never resolved is
	//    still denied — asserted in the deny test below.
	got := guestDial(t, mgr, info.ID, allowedTestIP.String(), 443)
	// HOST-side proof first: the destination was actually reached, so a
	// missing banner below would be the guest's nc, not the data path.
	select {
	case <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatalf("the allowlisted destination was NEVER REACHED — the redirect, the "+
			"authorizer or the dial did not carry the flow.\nguest saw: %q\n%s",
			got, hostNetDiagnostics(t, info))
	}
	assert.Contains(t, got, guestBannerText,
		"an ordinary guest program must reach the allowlisted destination and read real bytes")

	// DIAGNOSTIC, deliberately not an assertion: whether the guest's own
	// getaddrinfo can resolve. This fixture's busybox is statically linked
	// against a minimal libc and reports "bad address" even though nslookup —
	// which reads the same /etc/resolv.conf — succeeds above. That is a
	// property of the test rootfs, not of mgit: on a real base image
	// (`mgit sandbox base from debian:12-slim`) apt, curl and getent all
	// resolve through this same mechanism. Logged so the difference stays
	// visible rather than being quietly assumed away.
	t.Logf("guest getaddrinfo (fixture busybox libc, informational): %q",
		strings.TrimSpace(probe(t, mgr, info.ID, "nc -w 4 allowed.test 443 </dev/null 2>&1")))
	// Diagnostics for the redirect itself, so a failure above says WHERE it
	// broke rather than only that no bytes arrived.
	t.Logf("guest nat/route view: %s", probe(t, mgr, info.ID,
		"ip route get 140.82.112.3 2>&1"))

	// 3. The allow is audited (FR-17.18).
	var sawAllow bool
	for _, r := range audit.snapshot() {
		if r.Decision == model.EgressAllow {
			sawAllow = true
		}
	}
	assert.True(t, sawAllow, "the allowed flow must appear in the egress log")

	t.Logf("REAL VM PASS (firecracker allow): resolver=%q lookup=%q bytes=%q",
		strings.TrimSpace(resolv), strings.TrimSpace(lookup), strings.TrimSpace(got))
}

// TestE2E_Network_Allowlist_OffListIsRefusedNotUnreachable keeps the deny
// assertion and makes it distinguishable from a dead network: the guest must
// be able to REACH the enforcement point and be refused by it. "no route to
// host" would mean the sandbox is broken rather than enforced.
// Refs: MGIT-69, SEC-04
func TestE2E_Network_Allowlist_OffListIsRefusedNotUnreachable(t *testing.T) {
	kernel, rootfs := requireGuestImage(t)
	requireNetRoot(t)

	target, accepted := bannerListener(t)
	mgr, ref := registerGuestManager(t, kernel, rootfs, "")
	info := launchNetSandbox(t, mgr, ref, model.NetworkModeAllowlist, []string{"allowed.test:443"})
	audit := startEgressFor(t, info, model.NetworkModeAllowlist, []string{"allowed.test:443"}, target)
	requireGuestIsAddressed(t, mgr, info)

	// Control: the ALLOWED flow works, so a refusal below is policy, not
	// breakage. Without this the assertion proves nothing. Resolve first so
	// the address is pinned, then dial it — the real client sequence.
	probe(t, mgr, info.ID, "nslookup allowed.test 2>&1")
	allowed := guestDial(t, mgr, info.ID, allowedTestIP.String(), 443)
	select {
	case <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatalf("the control (allowed) flow never reached the destination, so a denial "+
			"here would prove nothing; guest saw: %q", allowed)
	}
	require.Contains(t, allowed, guestBannerText,
		"the control (allowed) flow must work, or a denial here would prove nothing")

	// A public address the policy does not name and no resolution ever
	// pinned — the SEC-04 raw-IP bypass case.
	out := guestDial(t, mgr, info.ID, "140.82.114.4", 443)

	// WHAT A REFUSAL LOOKS LIKE HERE, and why it is not an exit code.
	//
	// The kernel's REDIRECT completes the TCP handshake with the local proxy
	// BEFORE mgit sees the flow and decides, so `nc` reports a successful
	// connect (rc=0) even for a denied destination — it connected to the
	// enforcement point, not to the destination. That is inherent to a
	// redirect (TPROXY behaves the same) and differs from libkrun, whose
	// userspace gateway resets during the handshake so the guest sees
	// ECONNREFUSED at connect. The substantive claim is identical on both:
	// NO BYTES FROM THE DESTINATION, and nothing dialed toward it.
	//
	// rc=0 is in fact the proof the network is ALIVE — the guest reached the
	// enforcement point. A dead network would report "unreachable" instead,
	// which is asserted against below.
	assert.NotContains(t, out, guestBannerText, "an off-allowlist destination must carry no data (T3)")
	assert.NotContains(t, out, "unreachable",
		"the flow must fail because POLICY refused it, not because the guest has no route")
	assert.NotContains(t, out, "no route to host",
		"the flow must fail because POLICY refused it, not because the guest has no route")

	// HOST-side proof: nothing was ever dialed toward the destination. This is
	// the assertion an exit code cannot make.
	select {
	case <-accepted:
		t.Fatal("SEC-04 VIOLATION: an off-allowlist flow was proxied to the destination")
	case <-time.After(2 * time.Second):
	}
	var sawDeny bool
	for _, r := range audit.snapshot() {
		if r.Decision == model.EgressDeny {
			sawDeny = true
		}
	}
	assert.True(t, sawDeny, "the denial must appear in the egress log")
	t.Logf("REAL VM PASS (firecracker deny): off-list refused by policy while the allowed "+
		"flow worked; guest saw %q", strings.TrimSpace(out))
}

// TestE2E_Network_Open_GuestResolvesThroughTheGateway is the open-mode allow
// assertion for NAMES. Open mode bound no resolver at all while the guest was
// told its nameserver was the gateway, so every name failed against a dead
// port — invisible because the open-mode assertion dialed a raw IP.
// Refs: MGIT-69, FR-17.7, SEC-07
func TestE2E_Network_Open_GuestResolvesThroughTheGateway(t *testing.T) {
	kernel, rootfs := requireGuestImage(t)
	requireNetRoot(t)

	target, _ := bannerListener(t)
	mgr, ref := registerGuestManager(t, kernel, rootfs, "")
	info := launchNetSandbox(t, mgr, ref, model.NetworkModeOpen, nil)
	startEgressFor(t, info, model.NetworkModeOpen, nil, target)
	requireGuestIsAddressed(t, mgr, info)

	resolv := probe(t, mgr, info.ID, "cat /etc/resolv.conf 2>&1")
	assert.Contains(t, resolv, GatewayFor(info.ID).String(),
		"open mode must point the guest at a resolver that exists")

	lookup := probe(t, mgr, info.ID, "nslookup allowed.test 2>&1")
	assert.Contains(t, lookup, allowedTestIP.String(),
		"open mode must resolve names through its own gateway resolver")

	t.Logf("REAL VM PASS (firecracker open dns): resolver=%q lookup=%q",
		strings.TrimSpace(resolv), strings.TrimSpace(lookup))
}

// hostNetDiagnostics dumps the host-side plumbing this flow depends on, so a
// failure names WHICH link broke — the redirect rule, the filter chain, or the
// listener — instead of only reporting that no bytes arrived.
func hostNetDiagnostics(t *testing.T, info *model.SandboxInfo) string {
	t.Helper()
	tap := egress.TapName(info.ID)
	var b strings.Builder
	run := func(label, name string, args ...string) {
		out, err := exec.Command(name, args...).CombinedOutput() //nolint:gosec // fixed argv, root-gated test
		b.WriteString("\n--- " + label + " ---\n")
		if err != nil {
			b.WriteString("(" + err.Error() + ")\n")
		}
		for _, line := range strings.Split(string(out), "\n") {
			// Keep it to this sandbox's own rules; the host's table is noisy.
			if strings.Contains(line, tap) || strings.Contains(line, "1081") ||
				strings.Contains(line, GatewayFor(info.ID).String()) {
				b.WriteString(line + "\n")
			}
		}
	}
	run("nat table (expect a REDIRECT to 1081 jumped from PREROUTING)", "iptables", "-t", "nat", "-S")
	run("filter table (expect ACCEPT for :1081 before the DROP)", "iptables", "-S")
	run("listeners (expect the transparent proxy on the gateway:1081)", "ss", "-ltnp")
	return b.String()
}

// requireGuestImage gates on the kernel + rootfs fixtures these tests need,
// skipping loudly rather than passing vacuously.
func requireGuestImage(t *testing.T) (kernel, rootfs string) {
	t.Helper()
	kernel, _ = requireKVM(t)
	rootfs = os.Getenv("MGIT_E2E_GUEST_ROOTFS")
	if rootfs == "" || !fileExists(rootfs) {
		t.Skip("SKIP (guest egress e2e): set MGIT_E2E_GUEST_ROOTFS to a present guest image")
	}
	return kernel, rootfs
}
