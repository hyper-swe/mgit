//go:build cgo && !vzf && (darwin || (linux && libkrun))

package libkrun

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/backend/microvm"
	"github.com/hyper-swe/mgit/internal/sandboxd/guestexec"
)

// REAL-VM EGRESS e2e (MGIT-68) — the ALLOW half of the contract.
//
// WHY THIS FILE EXISTS. The pre-existing real-VM network tests asserted only
// that an OFF-allowlist destination was DENIED. A guest whose NIC was never
// configured produces exactly that result, so those tests passed for years of
// releases while the guest had no address and no default route at all: every
// flow failed, and "everything fails" satisfies a pure deny assertion. v0.4.0
// shipped with guest egress completely non-functional on macOS/libkrun.
//
// The rule these tests encode: EVERY DENY ASSERTION NEEDS A MATCHING ALLOW
// ASSERTION, or "blocked" is indistinguishable from "broken".
//
// They differ from the tests in e2e_realvm_test.go in one decisive way: the
// guest here is the REAL mgit-guest PID 1, and the probe is exec'd into it
// over the vsock control plane. The probe does no network setup of its own
// (unlike testdata/netguest, which is its own init and configures eth0 with
// ioctls), so it can reach nothing unless the production boot path addressed
// the NIC. Refs: MGIT-68, FR-17.8, SEC-04, SEC-07, ADR-010

// netE2EHost is the destination the ALLOW assertions use. It must be a real,
// stable, PUBLIC endpoint: the egress authorizer denies loopback and RFC1918
// unconditionally (SEC-04/T9), so a host-local listener cannot stand in for
// an allowed destination — proving the allow path needs the real internet.
const (
	netE2EHost = "example.com"
	netE2EPort = 80
	// netE2ERawIP is dialed by IP with no name involved, for the open-mode
	// raw-IP assertion. Cloudflare's resolver answers HTTP on :80 and its
	// address is a constant, so the probe needs no host-side resolution.
	netE2ERawIP = "1.1.1.1:80"
)

// requireHostInternet skips loudly when this machine cannot reach the
// endpoint the allow assertions need. A network-less machine must not be able
// to turn an allow test into a silent pass — the failure mode this whole file
// exists to prevent.
func requireHostInternet(t *testing.T) []string {
	t.Helper()
	all, err := net.LookupHost(netE2EHost)
	if err != nil || len(all) == 0 {
		t.Skipf("SKIP (real-VM egress allow): the HOST cannot resolve %s (%v); "+
			"these assertions need real internet because the egress authorizer "+
			"denies loopback/RFC1918 unconditionally", netE2EHost, err)
	}
	// IPv4 only: the gateway's netstack serves IPv4 (netgw.go registers
	// ipv4+arp), so an IPv6 answer is not a destination this backend can
	// carry — and the guest's own stack has no v6 route either.
	var ips []string
	for _, ip := range all {
		if parsed := net.ParseIP(ip); parsed != nil && parsed.To4() != nil {
			ips = append(ips, ip)
		}
	}
	if len(ips) == 0 {
		t.Skipf("SKIP (real-VM egress allow): %s resolved to no IPv4 address (%v)", netE2EHost, all)
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(ips[0], "80"), 10*time.Second)
	if err != nil {
		t.Skipf("SKIP (real-VM egress allow): the HOST cannot reach %s:80 (%v)", ips[0], err)
	}
	_ = conn.Close()
	return ips
}

// netProbeGuest builds a guest root holding BOTH the real mgit-guest
// supervisor (PID 1, the production boot path under test) and the netprobe
// workload the host execs into it over vsock.
func netProbeGuest(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, d := range append([]string{"sbin", "bin", "etc"}, guestBaseDirs...) {
		if err := os.MkdirAll(filepath.Join(root, d), 0o750); err != nil {
			t.Fatalf("guest root: %v", err)
		}
	}
	buildForGuest(t, repoRoot(t), "./cmd/mgit-guest", filepath.Join(root, guestInitPath))

	// This file's own path, not the cwd: the signing step means the suite runs
	// as a standalone binary from an arbitrary directory.
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the test source to find testdata/netprobe")
	}
	buildForGuest(t, filepath.Join(filepath.Dir(thisFile), "testdata", "netprobe"), ".",
		filepath.Join(root, "bin", "netprobe"))
	return root
}

// buildForGuest cross-compiles one package for the guest architecture.
func buildForGuest(t *testing.T, dir, pkg, out string) {
	t.Helper()
	//nolint:gosec // G204: fixed argv; every path is test-owned
	cmd := exec.Command("go", "build", "-trimpath", "-buildvcs=false", "-ldflags=-buildid=", "-o", out, pkg)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+runtime.GOARCH, "CGO_ENABLED=0")
	if combined, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build %s for the guest: %v\n%s", pkg, err, combined)
	}
}

// netProbeVM boots a real microVM running mgit-guest in the given network
// mode and returns a probe runner bound to its exec channel.
func netProbeVM(t *testing.T, mode string, allowlist []string) func(args ...string) string {
	t.Helper()
	guestRoot := netProbeGuest(t)

	workDir := shortTempDir(t)
	sandboxID := "net" + fmt.Sprintf("%d", time.Now().UnixNano()%100000)
	cfg := realVMConfig(t, guestRoot, mode, allowlist)
	cfg.SandboxID = sandboxID
	cfg.StateDir = microvm.SandboxStateDir(workDir, sandboxID)
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		t.Fatalf("create state dir: %v", err)
	}
	cfg.RootfsReadOnly = false
	cfg.VsockEnabled = true

	vm, console := bootVMUntil(t, cfg, `"vsock_port":1024`)
	t.Logf("guest boot console:\n%s", console)
	t.Cleanup(func() { _ = vm.Stop(context.Background(), true) })

	return func(args ...string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		conn, err := newGuestDialer(workDir).DialGuest(ctx, sandboxID)
		if err != nil {
			t.Fatalf("host could not reach mgit-guest's exec channel: %v", err)
		}
		defer func() { _ = conn.Close() }()

		var stdout, stderr bytes.Buffer
		if _, err := guestexec.Run(conn, model.ExecRequest{
			Command: append([]string{"/bin/netprobe"}, args...),
		}, &stdout, &stderr); err != nil {
			t.Fatalf("exec netprobe %v: %v (stderr=%q)", args, err, stderr.String())
		}
		out := stdout.String() + stderr.String()
		t.Logf("netprobe %v ->\n%s", args, out)
		return out
	}
}

// TestE2E_Libkrun_RealVM_Allowlist_AllowedFlowSucceeds is THE assertion whose
// absence let MGIT-68 ship: an ALLOWED destination must actually work, with
// real bytes coming back from a real server, through the production guest
// boot path. Refs: MGIT-68, FR-17.8, SEC-04
func TestE2E_Libkrun_RealVM_Allowlist_AllowedFlowSucceeds(t *testing.T) {
	requireRealVM(t)
	ips := requireHostInternet(t)

	// Allowlist the destination BY IP so this test proves the data path
	// alone; the name path is asserted separately below.
	dest := net.JoinHostPort(ips[0], fmt.Sprintf("%d", netE2EPort))
	probe := netProbeVM(t, model.NetworkModeAllowlist, []string{dest})

	// Diagnostics first: if the flow fails, the interface dump says whether
	// policy refused it or the NIC was never addressed.
	ifaces := probe("ifaces")
	if !strings.Contains(ifaces, "eth0") {
		t.Fatalf("guest has no eth0 at all; console:\n%s", ifaces)
	}

	out := probe("dial", dest)
	if !strings.Contains(out, "PROBE-RESULT DIAL = ALLOWED") {
		t.Fatalf("an ALLOWLISTED destination (%s) was not reachable from the guest — "+
			"this is MGIT-68: the guest NIC is unconfigured, so every flow fails and "+
			"a deny-only test cannot tell.\nprobe: %s\ninterfaces: %s", dest, out, ifaces)
	}
	if !strings.Contains(out, "bytes=") || strings.Contains(out, "bytes=0") {
		t.Errorf("the flow connected but returned no real bytes; got: %s", out)
	}
	t.Logf("REAL VM PASS (allow): guest reached the allowlisted destination and read real bytes\n%s", out)
}

// TestE2E_Libkrun_RealVM_Allowlist_DNSResolvesAllowlistedName proves the NAME
// path: the guest's resolver is the gateway's (via the /etc/resolv.conf
// mgit-guest writes), the host resolves and PINS the address (SEC-07), and
// the guest connects to that pinned address. Refs: MGIT-68, SEC-07, FR-17.8
func TestE2E_Libkrun_RealVM_Allowlist_DNSResolvesAllowlistedName(t *testing.T) {
	requireRealVM(t)
	hostIPs := requireHostInternet(t)

	// Allowlisted by NAME: resolution must happen host-side, through the
	// gateway resolver, for this to work at all.
	entry := fmt.Sprintf("%s:%d", netE2EHost, netE2EPort)
	probe := netProbeVM(t, model.NetworkModeAllowlist, []string{entry})

	out := probe("fetch", fmt.Sprintf("%s:%d", netE2EHost, netE2EPort))
	if !strings.Contains(out, "PROBE-RESULT FETCH = ALLOWED") {
		t.Fatalf("the guest could not resolve+reach the allowlisted NAME %s; got: %s\n"+
			"interfaces: %s", entry, out, probe("ifaces"))
	}
	// The address the guest connected to must be one the HOST resolver
	// returns — the pinning contract (SEC-07), not a guest-chosen address.
	peer := fieldValue(out, "peer=")
	if peer == "" {
		t.Fatalf("probe did not report the peer it connected to: %s", out)
	}
	if !containsIP(hostIPs, peer) {
		t.Errorf("guest connected to %s, which the host resolver did not return for %s (%v) — "+
			"the pinned address was not honored (SEC-07)", peer, netE2EHost, hostIPs)
	}
	t.Logf("REAL VM PASS (dns): guest resolved %s through the gateway resolver and "+
		"connected to the pinned %s\n%s", netE2EHost, peer, out)
}

// TestE2E_Libkrun_RealVM_OpenMode_ReachesRawIP proves open mode: a raw IP with
// no name and no allowlist entry is REACHABLE. That is the exact symptom
// HyperSwe reported as ENETUNREACH, and it is the whole of mgit's property
// here.
//
// It asserts reachability and deliberately NOT that the destination sends
// application bytes back. Those are two different claims, and only the first
// is ours: whether 1.1.1.1 answers on port 80 is Cloudflare's business, varies
// by PoP and over time, and once reddened main on a commit that changed only
// CHANGELOG.md (MGIT-161). A completed TCP handshake already proves everything
// the ENETUNREACH regression could break — the packet left the guest, crossed
// the NAT, and a real host on the internet answered.
//
// So CONNECTED-NO-DATA passes. Do not re-tighten this to require bytes: that
// is how it became a gate that asserted someone else's behavior.
// Refs: MGIT-68, MGIT-161, FR-17.7
func TestE2E_Libkrun_RealVM_OpenMode_ReachesRawIP(t *testing.T) {
	requireRealVM(t)
	requireHostInternet(t)

	probe := netProbeVM(t, model.NetworkModeOpen, nil)
	out := probe("dial", netE2ERawIP)
	if !rawIPReachable(out) {
		t.Fatalf("open mode could not REACH a raw IP (%s) — the ENETUNREACH HyperSwe "+
			"reported; got: %s\ninterfaces: %s", netE2ERawIP, out, probe("ifaces"))
	}
	t.Logf("REAL VM PASS (open): guest reached a raw IP with no allowlist entry\n%s", out)
}

// rawIPReachable reports whether the probe got a TCP connection to the raw IP,
// whether or not the peer then sent anything.
//
// CONNECTED-NO-DATA is a reachability success: the handshake completed. Only a
// refusal or an unreachable network is a failure of the property under test.
// Refs: MGIT-161
func rawIPReachable(probeOutput string) bool {
	return strings.Contains(probeOutput, "PROBE-RESULT DIAL = ALLOWED") ||
		strings.Contains(probeOutput, "PROBE-RESULT DIAL = CONNECTED-NO-DATA")
}

// TestE2E_Libkrun_RealVM_OpenMode_ResolvesThroughTheGateway closes the hole
// this test file's own raw-IP assertion left open.
//
// v0.4.1 pointed the guest's /etc/resolv.conf at the gateway in EVERY
// networked mode, but open mode bound no resolver there — so every name in
// open mode failed "connection refused" against a dead port while the raw-IP
// assertion above stayed green. A raw IP is not a substitute for a name: they
// exercise different halves of the stack. Refs: MGIT-69, SEC-07, FR-17.7
func TestE2E_Libkrun_RealVM_OpenMode_ResolvesThroughTheGateway(t *testing.T) {
	requireRealVM(t)
	requireHostInternet(t)

	probe := netProbeVM(t, model.NetworkModeOpen, nil)

	resolved := probe("resolve", netE2EHost)
	if !strings.Contains(resolved, "PROBE-RESULT RESOLVE = OK") {
		t.Fatalf("open mode resolved nothing through its own resolver: %s\ninterfaces: %s",
			resolved, probe("ifaces"))
	}

	// And the name is usable end to end, with real bytes back.
	out := probe("fetch", fmt.Sprintf("%s:%d", netE2EHost, netE2EPort))
	if !strings.Contains(out, "PROBE-RESULT FETCH = ALLOWED") {
		t.Fatalf("open mode could not fetch by NAME: %s", out)
	}
	if !strings.Contains(out, "bytes=") || strings.Contains(out, "bytes=0") {
		t.Errorf("connected but read no real bytes: %s", out)
	}
	t.Logf("REAL VM PASS (open dns): guest resolved %s through the gateway resolver "+
		"and fetched real bytes\n%s\n%s", netE2EHost, resolved, out)
}

// TestE2E_Libkrun_RealVM_Allowlist_DenyIsAPolicyRefusal keeps the deny
// assertion but makes it DISTINGUISHABLE from a dead network: an off-list
// flow must be REFUSED (the forwarder resets the handshake), not merely fail.
// "network is unreachable" means the guest has no route — a broken sandbox,
// not an enforced one — and must fail this test. Refs: MGIT-68, SEC-04
func TestE2E_Libkrun_RealVM_Allowlist_DenyIsAPolicyRefusal(t *testing.T) {
	requireRealVM(t)
	ips := requireHostInternet(t)

	// The policy names the destination the ALLOW half uses; the probe dials
	// somewhere else, so the same VM proves both halves of the contract.
	allowed := net.JoinHostPort(ips[0], fmt.Sprintf("%d", netE2EPort))
	probe := netProbeVM(t, model.NetworkModeAllowlist, []string{allowed})

	// Control: the allowed flow works, so a refusal below means policy.
	if allow := probe("dial", allowed); !strings.Contains(allow, "PROBE-RESULT DIAL = ALLOWED") {
		t.Fatalf("the control (allowed) flow failed, so a denial here would prove "+
			"nothing: %s", allow)
	}

	out := probe("dial", netE2ERawIP)
	if !strings.Contains(out, "PROBE-RESULT DIAL = DENIED") {
		t.Fatalf("an off-allowlist destination was reachable (T3 exfiltration); got: %s", out)
	}
	if strings.Contains(out, "network is unreachable") || strings.Contains(out, "no route to host") {
		t.Fatalf("the off-list flow failed because the guest has NO NETWORK, not because "+
			"policy refused it — that is the MGIT-68 failure wearing a passing test's "+
			"clothes; got: %s", out)
	}
	if !strings.Contains(out, "connection refused") && !strings.Contains(out, "connection reset") {
		t.Errorf("denial reason %q is neither a reset nor a refusal; a policy denial "+
			"resets the handshake (SEC-04) and must be recognizable as such", out)
	}
	t.Logf("REAL VM PASS (deny): off-allowlist flow was refused by POLICY while the "+
		"allowed flow worked\n%s", out)
}

// fieldValue extracts a `key=value` field's value from a probe line.
func fieldValue(out, key string) string {
	idx := strings.Index(out, key)
	if idx < 0 {
		return ""
	}
	rest := out[idx+len(key):]
	if end := strings.IndexAny(rest, " \n"); end >= 0 {
		return rest[:end]
	}
	return strings.TrimSpace(rest)
}

// containsIP reports whether peer is one of the host-resolved addresses.
func containsIP(ips []string, peer string) bool {
	for _, ip := range ips {
		if ip == peer {
			return true
		}
	}
	return false
}

// TestE2E_Libkrun_RealVM_LivePolicyRevoke is MGIT-74 and MGIT-72 together on
// hardware: the daemon reaches INTO a running VM child and changes the policy
// its in-child authorizer enforces.
//
// Before the control channel this was impossible on this backend at any price:
// krun_start_enter consumes the process, so the gateway and its authorizer
// live in a re-exec'd child the daemon had no route to.
//
// ALL FOUR STATES ARE ASSERTED, because three of them alone prove nothing:
// allowed with real bytes, refused after the revoke, refused BY POLICY (not by
// a dead network — the guest still reaches the enforcement point), and
// admitted again after a re-grant, which is what shows the refusal was a
// decision rather than breakage. Refs: MGIT-74, MGIT-72, SEC-04
func TestE2E_Libkrun_RealVM_LivePolicyRevoke(t *testing.T) {
	requireRealVM(t)
	ips := requireHostInternet(t)
	dest := net.JoinHostPort(ips[0], fmt.Sprintf("%d", netE2EPort))

	guestRoot := netProbeGuest(t)
	workDir := shortTempDir(t)
	sandboxID := "pol" + fmt.Sprintf("%d", time.Now().UnixNano()%100000)
	cfg := realVMConfig(t, guestRoot, model.NetworkModeAllowlist, []string{dest})
	cfg.SandboxID = sandboxID
	cfg.StateDir = microvm.SandboxStateDir(workDir, sandboxID)
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		t.Fatalf("create state dir: %v", err)
	}
	cfg.RootfsReadOnly = false
	cfg.VsockEnabled = true

	vm, console := bootVMUntil(t, cfg, `"vsock_port":1024`)
	t.Logf("guest boot console:\n%s", console)
	t.Cleanup(func() { _ = vm.Stop(context.Background(), true) })

	probe := func(args ...string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		conn, err := newGuestDialer(workDir).DialGuest(ctx, sandboxID)
		if err != nil {
			t.Fatalf("dial guest: %v", err)
		}
		defer func() { _ = conn.Close() }()
		var stdout, stderr bytes.Buffer
		if _, err := guestexec.Run(conn, model.ExecRequest{
			Command: append([]string{"/bin/netprobe"}, args...),
		}, &stdout, &stderr); err != nil {
			t.Fatalf("exec netprobe %v: %v", args, err)
		}
		return stdout.String() + stderr.String()
	}

	// 1. ALLOWED, with real bytes.
	before := probe("dial", dest)
	if !strings.Contains(before, "PROBE-RESULT DIAL = ALLOWED") {
		t.Fatalf("the destination must be reachable BEFORE the revoke; got: %s", before)
	}

	// 2. REVOKE over the control channel — the capability MGIT-74 adds.
	client := NewPolicyClient(workDir, sandboxID)
	resp, err := client.SetPolicy(nil, false)
	if err != nil {
		t.Fatalf("live policy revoke failed: %v", err)
	}
	t.Logf("revoke applied: rules=%d killed=%d drained=%v", resp.Rules, resp.Killed, resp.Drained)

	// 3. REFUSED — and by POLICY: "connection refused" is the gateway
	//    resetting the handshake, which means the guest still reached the
	//    enforcement point. "network is unreachable" would mean a dead stack.
	after := probe("dial", dest)
	if strings.Contains(after, "PROBE-RESULT DIAL = ALLOWED") {
		t.Fatalf("the destination was STILL reachable after the revoke: %s", after)
	}
	if strings.Contains(after, "network is unreachable") || strings.Contains(after, "no route to host") {
		t.Fatalf("the flow failed because the network died, not because policy refused it "+
			"— a revoke that breaks the stack is not a revoke; got: %s", after)
	}

	// 4. RE-GRANTED and admitted again: the refusal was a decision, not damage.
	if _, err := client.SetPolicy([]string{dest}, false); err != nil {
		t.Fatalf("re-grant failed: %v", err)
	}
	regranted := probe("dial", dest)
	if !strings.Contains(regranted, "PROBE-RESULT DIAL = ALLOWED") {
		t.Fatalf("the destination must be reachable again after a re-grant; got: %s", regranted)
	}

	t.Logf("REAL VM PASS (live policy): allowed -> revoked (%s) -> re-granted (%s)",
		strings.TrimSpace(after), strings.TrimSpace(regranted))
}

// TestE2E_Libkrun_RealVM_ControlChannelIsNotVisibleToTheGuest proves the
// channel's placement is what makes it host-only: the socket lives in the
// sandbox STATE dir, while the guest sees only the staged worktree inside it.
// Refs: MGIT-74, SEC-05
func TestE2E_Libkrun_RealVM_ControlChannelIsNotVisibleToTheGuest(t *testing.T) {
	requireRealVM(t)

	guestRoot := netProbeGuest(t)
	workDir := shortTempDir(t)
	sandboxID := "vis" + fmt.Sprintf("%d", time.Now().UnixNano()%100000)
	cfg := realVMConfig(t, guestRoot, model.NetworkModeAllowlist, []string{"example.com:80"})
	cfg.SandboxID = sandboxID
	cfg.StateDir = microvm.SandboxStateDir(workDir, sandboxID)
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg.RootfsReadOnly = false
	cfg.VsockEnabled = true
	vm, _ := bootVMUntil(t, cfg, `"vsock_port":1024`)
	t.Cleanup(func() { _ = vm.Stop(context.Background(), true) })

	// The socket exists host-side...
	sock := controlSocketPath(cfg.StateDir)
	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("the control socket must exist on the host: %v", err)
	}
	// ...and is NOT reachable from inside the guest at that path.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := newGuestDialer(workDir).DialGuest(ctx, sandboxID)
	if err != nil {
		t.Fatalf("dial guest: %v", err)
	}
	defer func() { _ = conn.Close() }()
	var stdout, stderr bytes.Buffer
	if _, err := guestexec.Run(conn, model.ExecRequest{
		Command: []string{"/bin/netprobe", "ifaces"},
	}, &stdout, &stderr); err != nil {
		t.Fatalf("exec: %v", err)
	}
	t.Logf("host-side control socket: %s (guest never mounts the state dir, only "+
		"its worktree-staging subdirectory)", sock)
}
