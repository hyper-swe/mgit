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

// allowedTestIP is a TEST-NET-3 address (RFC 5737, documentation range): it is
// public (not RFC1918/loopback/link-local), so the egress authorizer does not
// unconditionally deny it, yet it is never a real destination — the injected
// proxy Dial routes it to a local listener. Refs: SEC-04
var allowedTestIP = netip.MustParseAddr("203.0.113.10")

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

	// DNS: allowlisted name resolves; non-allowlisted is refused.
	okDNS := guestProbe(t, mgr, info.ID, up+"nslookup allowed.test "+gw.String()+" 2>&1")
	assert.Contains(t, string(okDNS.Stdout)+string(okDNS.Stderr), allowedTestIP.String(),
		"an allowlisted name resolves through the host DNS")
	badDNS := guestProbe(t, mgr, info.ID, up+"nslookup denied.test "+gw.String()+" 2>&1; echo rc=$?")
	assert.NotContains(t, string(badDNS.Stdout), allowedTestIP.String())
	assert.NotContains(t, string(badDNS.Stdout), "rc=0", "a non-allowlisted name is refused")

	// Proxy: a CONNECT to the allowlisted name succeeds (200); a non-allowlisted
	// name is refused (no 200).
	connect := func(host string) string {
		script := up + fmt.Sprintf(
			"printf 'CONNECT %s:443 HTTP/1.1\\r\\nHost: %s\\r\\n\\r\\n' | nc -w 6 %s %d",
			host, host, gw.String(), hostProxyPort)
		r := guestProbe(t, mgr, info.ID, script)
		return string(r.Stdout) + string(r.Stderr)
	}
	assert.Contains(t, connect("allowed.test"), "200", "allowlisted CONNECT is established")
	assert.NotContains(t, connect("denied.test"), "200", "non-allowlisted CONNECT is refused")

	// A raw-IP direct connect (bypassing the proxy) is dropped by the tap
	// firewall's default-deny — the guest cannot reach an arbitrary IP directly.
	raw := guestProbe(t, mgr, info.ID, up+"nc -w 3 203.0.113.30 443 </dev/null; echo rc=$?")
	assert.NotContains(t, string(raw.Stdout), "rc=0",
		"a direct raw-IP egress is dropped (only the proxy/DNS are reachable)")

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

// TestE2E_Network_Open_NATEgress proves open mode no longer fails closed once
// the external NAT interface is wired (the MGIT-11.13.6 extIface fix): the
// guest launches with a NIC and reaches a host service via NAT through the
// auto-detected default-route interface. Refs: FR-17.7
func TestE2E_Network_Open_NATEgress(t *testing.T) {
	kernel, _ := requireKVM(t)
	requireNetRoot(t)
	rootfs := os.Getenv("MGIT_E2E_GUEST_ROOTFS")
	if rootfs == "" || !fileExists(rootfs) {
		t.Skip("set MGIT_E2E_GUEST_ROOTFS to a present guest image")
	}

	extIP, extIface := hostExternalAddr(t)

	// A host listener on the external-interface IP: the guest, NATed out that
	// interface, must be able to reach it (an "arbitrary host" open mode allows
	// but allowlist/none would block).
	ln, err := net.Listen("tcp", net.JoinHostPort(extIP, "0"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	reached := make(chan struct{}, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		select {
		case reached <- struct{}{}:
		default:
		}
		_ = c.Close()
	}()
	_, portStr, err := net.SplitHostPort(ln.Addr().String())
	require.NoError(t, err)

	// Open mode auto-detects the default route; launch must SUCCEED (with the
	// old extIface="" it failed closed at TapPlan.validate).
	mgr, ref := registerGuestManager(t, kernel, rootfs, extIface)
	info := launchNetSandbox(t, mgr, ref, model.NetworkModeOpen, nil)

	up := netUpPrefix(info.ID)
	probe := guestProbe(t, mgr, info.ID,
		up+fmt.Sprintf("nc -w 6 %s %s </dev/null; echo rc=$?", extIP, portStr))
	// Either the listener observed the connection, or nc reported success.
	select {
	case <-reached:
	case <-time.After(2 * time.Second):
		assert.Contains(t, string(probe.Stdout), "rc=0",
			"open mode reaches an arbitrary host via NAT (extIface wired)")
	}
}

// hostExternalAddr returns the host's default-route interface and its first
// IPv4 address — the open-mode NAT egress path. Derived at runtime (never
// logged or committed). Skips if the host has no usable external IPv4.
func hostExternalAddr(t *testing.T) (ip, iface string) {
	t.Helper()
	name, err := defaultRouteIface()
	if err != nil {
		t.Skipf("no default-route interface to NAT through: %v", err)
	}
	ni, err := net.InterfaceByName(name)
	if err != nil {
		t.Skipf("default-route interface %s not resolvable: %v", name, err)
	}
	addrs, err := ni.Addrs()
	require.NoError(t, err)
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
			return ipn.IP.String(), name
		}
	}
	t.Skipf("default-route interface %s has no IPv4 address", name)
	return "", ""
}
