//go:build linux

// Live network-enforcement round-trips against a real mgit-guest on KVM
// (MGIT-11.7 / FR-17.7, FR-17.8). These boot a signed guest and probe egress
// from inside it with the busybox net applets (nc/nslookup), proving the
// host-side tap firewall + egress proxy + restricted DNS actually allow and
// deny the right flows — not just that the engine compiles.
//
// Gating: like the exec/land e2e they need /dev/kvm + firecracker + a guest
// rootfs (MGIT_E2E_GUEST_ROOTFS). The allowlist and open modes additionally
// create a host tap + iptables rules, so they need root (CAP_NET_ADMIN) and
// skip otherwise. The allowlist target is kept hermetic: the resolver Lookup
// and proxy Dial are injected so an allowlisted name maps to a TEST-NET-3
// address served by a local listener — no real internet egress in the test.
// Refs: FR-17.7, FR-17.8, SEC-04, SEC-07, MGIT-11.13.6
package firecracker

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"log/slog"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/backend/microvm"
	"github.com/hyper-swe/mgit/internal/sandboxd/egress"
	"github.com/hyper-swe/mgit/internal/sandboxd/images"
)

// allowedTestIP is a genuinely public, routable address (GitHub's range) so it
// passes the authorizer's unconditional-deny check — TEST-NET/documentation
// ranges are themselves denied (egress.deniedPrefixes), which is why a doc IP
// would resolve via DNS but be refused by the proxy. No real connection is
// made: the injected proxy Dial routes the authorized flow to a local
// listener. Refs: SEC-04
var allowedTestIP = netip.MustParseAddr("140.82.112.3")

// requireNetRoot skips a privileged network test when not running as root: the
// tap + iptables setup needs CAP_NET_ADMIN.
func requireNetRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("network enforcement e2e needs root (tap + iptables); run with sudo -E")
	}
}

// recordingEgressAudit is an in-memory egress.Auditor: it captures every
// allow/deny the proxy and resolver record so a test can assert the
// sandbox_egress_log contents. Refs: FR-17.8
type recordingEgressAudit struct {
	mu      sync.Mutex
	records []model.EgressRecord
}

func (a *recordingEgressAudit) AppendEgressRecord(_ context.Context, rec *model.EgressRecord) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.records = append(a.records, *rec)
	return nil
}

func (a *recordingEgressAudit) snapshot() []model.EgressRecord {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]model.EgressRecord, len(a.records))
	copy(out, a.records)
	return out
}

// registerGuestManager registers the signed mgit-guest image in a fresh trust
// root and returns a firecracker manager bound to it plus the pinned image
// ref. extIface is the open-mode NAT interface (empty auto-detects the host
// default route).
func registerGuestManager(t *testing.T, kernel, rootfs, extIface string) (*microvm.Manager, string) {
	t.Helper()
	clock := func() time.Time { return time.Now().UTC() }
	hostRoot := t.TempDir()
	_, err := images.GenerateTrustRoot(context.Background(), hostRoot, noopAudit{})
	require.NoError(t, err)
	priv, err := images.LoadSigningKey(hostRoot)
	require.NoError(t, err)
	entry, err := images.BuildEntry(kernel, rootfs, e2eGuestCmdline)
	require.NoError(t, err)
	ref, err := images.Register(hostRoot, "mgit-guest", entry, priv)
	require.NoError(t, err)
	store, err := images.NewStore(hostRoot, clock)
	require.NoError(t, err)

	workDir, err := os.MkdirTemp("", "mgnet") // short path for the vsock socket
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(workDir) })

	mgr, err := NewManager(Config{
		WorkDir:  workDir,
		ExtIface: extIface,
		Resolve: func(r string) (ImagePaths, error) {
			ri, rerr := store.Resolve(r)
			return ImagePaths{KernelPath: ri.KernelPath, RootfsPath: ri.RootfsPath, Cmdline: ri.Cmdline}, rerr
		},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Clock:  clock,
	})
	require.NoError(t, err)
	return mgr, ref
}

// launchNetSandbox boots a sandbox in the given network mode and returns its
// host-assigned info. The caller supplies the pinned image ref.
func launchNetSandbox(t *testing.T, mgr *microvm.Manager, ref, mode string, allowlist []string) *model.SandboxInfo {
	t.Helper()
	wtPath := filepath.Join(t.TempDir(), "repo", "wt")
	require.NoError(t, os.MkdirAll(wtPath, 0o750))
	info, err := mgr.Launch(context.Background(), model.SandboxLaunchOptions{
		TaskID:       "MGIT-11.13.6",
		WorktreePath: wtPath,
		ImageRef:     ref,
		Network:      model.NetworkPolicy{Mode: mode, Allowlist: allowlist},
		CPUs:         1, MemoryMB: 256,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Remove(context.Background(), info.ID, true) })
	return info
}

// netUpPrefix is a shell snippet that brings the guest NIC up with its static
// /30 and default route, so a probe works regardless of whether the kernel did
// IP autoconfig. Derived from the host-owned sandbox ID (never guest input).
func netUpPrefix(sandboxID string) string {
	gw, guest, _ := subnetFor(sandboxID)
	return fmt.Sprintf(
		"ip link set eth0 up 2>/dev/null; ip addr add %s/30 dev eth0 2>/dev/null; "+
			"ip route replace default via %s 2>/dev/null; ", guest, gw)
}

// guestProbe execs one shell probe in the guest, retrying until the guest is
// serving vsock (boot is async). It returns the exec result.
func guestProbe(t *testing.T, mgr *microvm.Manager, id, script string) *model.ExecResult {
	t.Helper()
	var (
		res *model.ExecResult
		err error
	)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		res, err = mgr.Exec(context.Background(), id, model.ExecRequest{
			Command: []string{"/bin/sh", "-c", script},
		})
		if err == nil {
			return res
		}
		time.Sleep(400 * time.Millisecond)
	}
	require.NoError(t, err, "exec must reach the guest once it serves vsock")
	return res
}

// TestE2E_Network_None_NoEgress proves none mode attaches no NIC: the guest has
// only loopback and cannot egress. Rootless (no tap/iptables). Refs: FR-17.7
func TestE2E_Network_None_NoEgress(t *testing.T) {
	kernel, _ := requireKVM(t)
	rootfs := os.Getenv("MGIT_E2E_GUEST_ROOTFS")
	if rootfs == "" {
		t.Skip("set MGIT_E2E_GUEST_ROOTFS to run the network round-trips")
	}
	if !fileExists(rootfs) {
		t.Skipf("guest rootfs %s absent", rootfs)
	}
	mgr, ref := registerGuestManager(t, kernel, rootfs, "")
	info := launchNetSandbox(t, mgr, ref, model.NetworkModeNone, nil)

	// No NIC: the only link is loopback, and a connect attempt cannot leave.
	links := guestProbe(t, mgr, info.ID, "ip -o link show 2>/dev/null | awk -F': ' '{print $2}'")
	assert.NotContains(t, string(links.Stdout), "eth0", "none mode attaches no NIC")

	egr := guestProbe(t, mgr, info.ID, "nc -w 3 203.0.113.30 443 </dev/null; echo rc=$?")
	assert.Contains(t, string(egr.Stdout), "rc=", "probe ran")
	assert.NotContains(t, string(egr.Stdout), "rc=0", "no egress is possible without a NIC")
}

// TestE2E_Network_Allowlist_ProxyAndDNSEnforced proves the headline allowlist
// enforcement on a live guest: an allowlisted name resolves via the host DNS
// and connects through the host CONNECT proxy, while a non-allowlisted name is
// refused at both, a raw-IP direct connect is dropped by the tap firewall, and
// every proxy/DNS decision is audited. Refs: FR-17.7, FR-17.8, SEC-04, SEC-07
func TestE2E_Network_Allowlist_ProxyAndDNSEnforced(t *testing.T) {
	kernel, _ := requireKVM(t)
	requireNetRoot(t)
	rootfs := os.Getenv("MGIT_E2E_GUEST_ROOTFS")
	if rootfs == "" || !fileExists(rootfs) {
		t.Skip("set MGIT_E2E_GUEST_ROOTFS to a present guest image")
	}

	// A local listener stands in for the allowlisted public destination; the
	// injected proxy Dial routes the authorized flow here (hermetic).
	target, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = target.Close() })
	go func() {
		for {
			c, err := target.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(io.Discard, c); _ = c.Close() }()
		}
	}()

	audit := &recordingEgressAudit{}
	runner, err := egress.NewRunner(egress.RunnerConfig{
		Audit: audit,
		// Allowlisted name resolves to the TEST-NET address; anything else NXDOMAIN.
		Lookup: func(_ context.Context, name string) ([]netip.Addr, error) {
			if name == "allowed.test" {
				return []netip.Addr{allowedTestIP}, nil
			}
			return nil, egress.ErrNXDOMAIN
		},
		// Authorized flows dial the local stand-in target.
		Dial: func(ctx context.Context, _ netip.Addr, _ int) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", target.Addr().String())
		},
		Clock:     func() time.Time { return time.Now().UTC() },
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		ProxyPort: hostProxyPort,
		DNSPort:   hostDNSPort,
	})
	require.NoError(t, err)

	mgr, ref := registerGuestManager(t, kernel, rootfs, "")
	info := launchNetSandbox(t, mgr, ref, model.NetworkModeAllowlist, []string{"allowed.test"})

	// The tap + gateway IP exist after launch; bind the proxy + DNS there.
	_, err = runner.Start(context.Background(), egress.Binding{
		SandboxID: info.ID, TaskID: info.TaskID,
		GatewayIP: GatewayFor(info.ID),
		Policy:    model.NetworkPolicy{Mode: model.NetworkModeAllowlist, Allowlist: []string{"allowed.test"}},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = runner.Stop(info.ID) })

	gw, _, _ := subnetFor(info.ID)
	up := netUpPrefix(info.ID)

	// SEC-04 (F-B): the guest kernel boots with ipv6.disable=1, so there is no
	// IPv6 stack and thus no un-firewalled v6 egress path (the tap firewall is
	// IPv4-only). The guest must have no IPv6 address or route on its NIC.
	v6 := guestProbe(t, mgr, info.ID, up+"ip -6 addr show dev eth0 2>&1; ip -6 route 2>&1")
	assert.NotContains(t, string(v6.Stdout)+string(v6.Stderr), "inet6",
		"IPv6 is disabled in the guest (ipv6.disable=1) — no v6 egress path")

	// From the GUEST: DNS resolution of an allowlisted name succeeds through the
	// host resolver on the gateway; a non-allowlisted name is REFUSED (the
	// resolver answers only allowlisted names). busybox nslookup exits 0 even on
	// REFUSED, so assert on the response text, not the exit code.
	okDNS := guestProbe(t, mgr, info.ID, up+"nslookup allowed.test "+gw.String()+" 2>&1")
	assert.Contains(t, string(okDNS.Stdout)+string(okDNS.Stderr), allowedTestIP.String(),
		"an allowlisted name resolves through the host DNS")
	badDNS := guestProbe(t, mgr, info.ID, up+"nslookup denied.test "+gw.String()+" 2>&1")
	out := string(badDNS.Stdout) + string(badDNS.Stderr)
	assert.NotContains(t, out, allowedTestIP.String())
	assert.Contains(t, out, "REFUSED", "a non-allowlisted name is refused by the host resolver")

	// From the GUEST: the firewall lets the guest reach the proxy port (a TCP
	// connect to gateway:proxy succeeds) but DROPS a direct connect to an
	// arbitrary IP (default-deny) — so the proxy is the only way out.
	reach := guestProbe(t, mgr, info.ID,
		up+fmt.Sprintf("nc -w 4 %s %d </dev/null; echo rc=$?", gw.String(), hostProxyPort))
	assert.Contains(t, string(reach.Stdout), "rc=0",
		"the guest can reach the host proxy port (firewall allows gateway:proxy)")
	raw := guestProbe(t, mgr, info.ID, up+"nc -w 3 203.0.113.30 443 </dev/null; echo rc=$?")
	assert.NotContains(t, string(raw.Stdout), "rc=0",
		"a direct raw-IP egress is dropped (only the proxy/DNS are reachable)")

	// Proxy enforcement: the proxy speaks a length-prefixed binary protocol (not
	// HTTP CONNECT), so drive it directly over the gateway-bound listener — the
	// same socket the guest reaches — and assert it authorizes the allowlisted
	// destination and refuses the non-allowlisted one. Refs: SEC-04
	proxyConnect := func(host string) (bool, error) {
		c, derr := net.DialTimeout("tcp", net.JoinHostPort(gw.String(), strconv.Itoa(hostProxyPort)), 5*time.Second)
		if derr != nil {
			return false, derr
		}
		defer func() { _ = c.Close() }()
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		if err := egress.EncodeConnectRequest(c, egress.ConnectRequest{Protocol: "tcp", Host: host, Port: 443}); err != nil {
			return false, err
		}
		allow, _, err := egress.DecodeConnectReply(c)
		return allow, err
	}
	allowOK, err := proxyConnect("allowed.test")
	require.NoError(t, err)
	assert.True(t, allowOK, "the proxy authorizes an allowlisted CONNECT")
	denyOK, err := proxyConnect("denied.test")
	require.NoError(t, err)
	assert.False(t, denyOK, "the proxy refuses a non-allowlisted CONNECT")

	// Every proxy/DNS decision is audited (allow + deny both present).
	recs := audit.snapshot()
	var sawAllow, sawDeny bool
	for _, r := range recs {
		switch r.Decision {
		case model.EgressAllow:
			sawAllow = true
		case model.EgressDeny:
			sawDeny = true
		}
	}
	assert.True(t, sawAllow, "an allow decision was recorded in the egress log")
	assert.True(t, sawDeny, "a deny decision was recorded in the egress log")
}

// openEgressTarget is a well-known public host:port the open-mode guest NATs
// out to. Port 443 is used because the dev host already egresses on it (it
// fetches from GitHub), so it is the most reliable "arbitrary external host"
// reachability check that does not depend on the box's DNS or UDP egress.
const openEgressTarget = "1.1.1.1 443"

// TestE2E_Network_Open_NATEgress proves open mode no longer fails closed once
// the external NAT interface is wired (the MGIT-11.13.6 extIface fix): launch
// succeeds with the auto-detected default-route interface (previously it
// failed at TapPlan.validate with extIface=""), and the guest reaches an
// arbitrary external host via host NAT — egress that none/allowlist would
// block. Refs: FR-17.7
func TestE2E_Network_Open_NATEgress(t *testing.T) {
	kernel, _ := requireKVM(t)
	requireNetRoot(t)
	rootfs := os.Getenv("MGIT_E2E_GUEST_ROOTFS")
	if rootfs == "" || !fileExists(rootfs) {
		t.Skip("set MGIT_E2E_GUEST_ROOTFS to a present guest image")
	}

	// extIface left empty so the manager auto-detects the host default route;
	// launch SUCCEEDING in open mode is itself the extIface-wiring fix.
	mgr, ref := registerGuestManager(t, kernel, rootfs, "")
	info := launchNetSandbox(t, mgr, ref, model.NetworkModeOpen, nil)

	up := netUpPrefix(info.ID)
	probe := guestProbe(t, mgr, info.ID,
		up+fmt.Sprintf("nc -w 8 %s </dev/null; echo rc=$?", openEgressTarget))
	assert.Contains(t, string(probe.Stdout), "rc=0",
		"open mode reaches an arbitrary external host via host NAT (extIface wired)")
}
