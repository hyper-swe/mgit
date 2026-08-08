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
	"io"
	"net"
	"net/netip"
	"os"
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
func bannerListener(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				_, _ = io.WriteString(c, guestBannerText+"\n")
				_ = c.Close()
			}()
		}
	}()
	return ln
}

// startEgressFor brings up the host egress stack for a launched sandbox,
// hermetically: an allowlisted name resolves to a public-looking address and
// authorized flows are dialled to the local banner listener.
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

	target := bannerListener(t)
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

	// 2. An ORDINARY TCP connection to that name must carry REAL BYTES. This
	//    is the assertion whose absence let allowlist mode ship unusable: the
	//    guest speaks no proxy protocol, and before the transparent redirect
	//    it had no way through the policy at all.
	got := probe(t, mgr, info.ID, "nc -w 6 allowed.test 443 </dev/null 2>&1")
	assert.Contains(t, got, guestBannerText,
		"an ordinary guest program must reach the allowlisted destination and read real bytes")

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

	target := bannerListener(t)
	mgr, ref := registerGuestManager(t, kernel, rootfs, "")
	info := launchNetSandbox(t, mgr, ref, model.NetworkModeAllowlist, []string{"allowed.test:443"})
	audit := startEgressFor(t, info, model.NetworkModeAllowlist, []string{"allowed.test:443"}, target)
	requireGuestIsAddressed(t, mgr, info)

	// Control: the ALLOWED flow works, so a refusal below is policy, not
	// breakage. Without this the assertion proves nothing.
	allowed := probe(t, mgr, info.ID, "nc -w 6 allowed.test 443 </dev/null 2>&1")
	require.Contains(t, allowed, guestBannerText,
		"the control (allowed) flow must work, or a denial here would prove nothing")

	// A public address the policy does not name.
	out := probe(t, mgr, info.ID, "nc -w 6 140.82.114.4 443 </dev/null 2>&1; echo rc=$?")

	assert.NotContains(t, out, guestBannerText, "an off-allowlist destination must carry no data (T3)")
	assert.NotContains(t, out, "rc=0", "an off-allowlist flow must not succeed")
	assert.NotContains(t, out, "unreachable",
		"the flow must fail because POLICY refused it, not because the guest has no route")
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
// port — invisible because the open-mode assertion dialled a raw IP.
// Refs: MGIT-69, FR-17.7, SEC-07
func TestE2E_Network_Open_GuestResolvesThroughTheGateway(t *testing.T) {
	kernel, rootfs := requireGuestImage(t)
	requireNetRoot(t)

	target := bannerListener(t)
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
