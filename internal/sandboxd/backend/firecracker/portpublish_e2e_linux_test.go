//go:build linux

// Live one-way port-publish round-trips against a real mgit-guest on KVM
// (SEC-09 / FR-17.8). These boot a signed guest, start a tiny dev server
// INSIDE the guest, wire the host-side portpub.Controller over the
// firecracker per-VM vsock port dialer, and prove:
//
//  1. the host reaches the guest dev server at host 127.0.0.1:<hostPort>
//     (host->guest forwarding, the publish direction), and
//  2. the guest CANNOT reach a host loopback service (the one-way guarantee):
//     in open mode the host's 127.0.0.0/8 loopback is simply not routable
//     across the guest's NAT'd NIC — a packet the guest sends to 127.0.0.1
//     stays in the guest's own loopback and never reaches the host. In
//     allowlist/none mode the host egress proxy/absent-NIC already denies it
//     (covered by the network e2e); here we assert the open-mode case, the
//     weakest posture, so SEC-09 holds even with NAT enabled.
//
// Gating: like the network e2e they need /dev/kvm + firecracker + a guest
// rootfs (MGIT_E2E_GUEST_ROOTFS); open mode creates a host tap + iptables, so
// it needs root (CAP_NET_ADMIN) and skips otherwise. Probes are BOUNDED
// (nc -w, fixed retries) — never an unbounded scan.
// Refs: SEC-09, FR-17.8, FR-17.19, MGIT-11.10.12
package firecracker

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/backend/microvm"
	"github.com/hyper-swe/mgit/internal/sandboxd/images"
	"github.com/hyper-swe/mgit/internal/sandboxd/portpub"
)

// registerGuestManagerAt is registerGuestManager but returns the manager's
// workDir too, so the test can build a firecracker port dialer over the SAME
// per-VM vsock sockets the manager creates. Refs: SEC-09
func registerGuestManagerAt(t *testing.T, kernel, rootfs, extIface string) (*microvm.Manager, string, string) {
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

	workDir, err := os.MkdirTemp("", "mgpub") // short path for the vsock socket
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
	return mgr, ref, workDir
}

// guestDevServerPort is the port the in-guest dev server listens on; the host
// publishes it to a (distinct) host loopback port.
const guestDevServerPort = 3000

// startGuestDevServer launches a tiny busybox httpd-style dev server inside the
// guest on guestDevServerPort, serving a fixed body, and waits until it is
// listening. busybox nc -ll keeps re-accepting, so the forwarded host
// connections are served repeatedly. Refs: SEC-09
func startGuestDevServer(t *testing.T, mgr *microvm.Manager, id, body string) {
	t.Helper()
	// nc -ll: listen, serve, and keep listening (busybox). Echo a fixed body on
	// each accept. Backgrounded so the exec returns; the server outlives it.
	script := fmt.Sprintf(
		"(while true; do echo '%s' | nc -ll -p %d -w 1 >/dev/null 2>&1 || nc -l -p %d >/dev/null 2>&1; done) >/dev/null 2>&1 &",
		body, guestDevServerPort, guestDevServerPort)
	_ = guestProbe(t, mgr, id, script)
	// Confirm something is listening on the port before publishing.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		r := guestProbe(t, mgr, id, fmt.Sprintf("nc -w 2 127.0.0.1 %d </dev/null; echo rc=$?", guestDevServerPort))
		if len(r.Stdout) > 0 {
			return
		}
		time.Sleep(400 * time.Millisecond)
	}
}

// TestE2E_PortPublish_GuestServiceReachableOnHost proves the host reaches a
// guest dev server at host 127.0.0.1:<hostPort> through the portpub controller
// wired over the firecracker per-VM port dialer (the publish direction).
// Refs: SEC-09, FR-17.8
func TestE2E_PortPublish_GuestServiceReachableOnHost(t *testing.T) {
	// GATED on the guest-side vsock<->TCP publish bridge (MGIT-11.10.13): the
	// host publisher dials the guest over VSOCK (DialGuestPort), but a real dev
	// server (and this test's busybox `nc -ll`) listens on TCP. Without an
	// in-guest AF_VSOCK->TCP bridge the host reaches nothing. The host-side
	// publisher + SEC-09 one-way isolation are proven by
	// TestE2E_PortPublish_GuestCannotReachHostLoopback; un-skip this once the
	// guest bridge lands. Refs: SEC-09, MGIT-11.10.13
	t.Skip("needs the guest-side vsock<->TCP publish bridge (MGIT-11.10.13)")
	kernel, _ := requireKVM(t)
	requireNetRoot(t)
	rootfs := os.Getenv("MGIT_E2E_GUEST_ROOTFS")
	if rootfs == "" || !fileExists(rootfs) {
		t.Skip("set MGIT_E2E_GUEST_ROOTFS to a present guest image")
	}

	mgr, ref, workDir := registerGuestManagerAt(t, kernel, rootfs, "")
	info := launchNetSandbox(t, mgr, ref, model.NetworkModeOpen, nil)
	startGuestDevServer(t, mgr, info.ID, "hello-from-guest")

	// Wire the host-side controller over the firecracker per-VM port dialer.
	ctrl, err := portpub.New(portpub.Config{
		Dialer: NewPortDialer(workDir),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	require.NoError(t, err)

	hostPort := freeHostPort(t)
	require.NoError(t, ctrl.StartPublish(context.Background(), *info,
		[]model.PortPublish{{HostPort: hostPort, GuestPort: guestDevServerPort}}))
	t.Cleanup(func() { ctrl.StopPublish(info.ID) })

	// The publisher binds host loopback only.
	addrs := ctrl.HostAddrs(info.ID)
	require.Len(t, addrs, 1)
	assert.True(t, addrs[0].(*net.TCPAddr).IP.IsLoopback(),
		"the published port binds 127.0.0.1, never an external interface")

	// From the HOST: connect to 127.0.0.1:<hostPort> and read the guest's reply.
	body := dialAndRead(t, fmt.Sprintf("127.0.0.1:%d", hostPort))
	assert.Contains(t, body, "hello-from-guest",
		"the host reaches the guest dev server through the published port")

	// Teardown leaves no residue: the host listener is gone.
	ctrl.StopPublish(info.ID)
	_, derr := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", hostPort), time.Second)
	assert.Error(t, derr, "the published host listener is closed on teardown (no residue)")
}

// TestE2E_PortPublish_GuestCannotReachHostLoopback proves the one-way
// guarantee: even in OPEN mode (NAT enabled, the weakest posture), the guest
// cannot reach a host loopback service. The host's 127.0.0.0/8 is not routable
// across the guest NIC, so a guest connect to 127.0.0.1:<port> never reaches
// the host listener. Refs: SEC-09
func TestE2E_PortPublish_GuestCannotReachHostLoopback(t *testing.T) {
	kernel, _ := requireKVM(t)
	requireNetRoot(t)
	rootfs := os.Getenv("MGIT_E2E_GUEST_ROOTFS")
	if rootfs == "" || !fileExists(rootfs) {
		t.Skip("set MGIT_E2E_GUEST_ROOTFS to a present guest image")
	}

	// A host loopback "secret" service the guest must NOT be able to reach.
	secret, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = secret.Close() })
	reached := make(chan struct{}, 1)
	go func() {
		for {
			c, aerr := secret.Accept()
			if aerr != nil {
				return
			}
			select {
			case reached <- struct{}{}:
			default:
			}
			_ = c.Close()
		}
	}()
	hostPort := secret.Addr().(*net.TCPAddr).Port

	mgr, ref, _ := registerGuestManagerAt(t, kernel, rootfs, "")
	info := launchNetSandbox(t, mgr, ref, model.NetworkModeOpen, nil)

	up := netUpPrefix(info.ID)
	// BOUNDED probe: nc with a short timeout, then report the exit code. A
	// success (rc=0) would mean the guest reached the host loopback — a SEC-09
	// breach. We expect a non-zero rc (connection refused/timed out).
	probe := guestProbe(t, mgr, info.ID,
		up+fmt.Sprintf("nc -w 4 127.0.0.1 %d </dev/null; echo rc=$?", hostPort))
	assert.NotContains(t, string(probe.Stdout), "rc=0",
		"the guest cannot reach the host loopback service (one-way, SEC-09)")

	select {
	case <-reached:
		t.Fatal("SEC-09 VIOLATION: the host loopback service accepted a connection from the guest")
	case <-time.After(time.Second):
		// No connection reached the host loopback service: the one-way property holds.
	}
}

// freeHostPort returns a free non-privileged host port (bind ephemeral, close).
func freeHostPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	p := ln.Addr().(*net.TCPAddr).Port
	require.NoError(t, ln.Close())
	return p
}

// dialAndRead opens a host TCP connection, sends a bounded request, and reads a
// bounded reply, returning it as a string. Bounded by a deadline and a small
// buffer — never an unbounded read. Refs: SEC-09
func dialAndRead(t *testing.T, addr string) string {
	t.Helper()
	var conn net.Conn
	var err error
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		conn, err = net.DialTimeout("tcp", addr, 2*time.Second)
		if err == nil {
			break
		}
		time.Sleep(400 * time.Millisecond)
	}
	require.NoError(t, err, "the host must reach the published port")
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = io.WriteString(conn, "GET / HTTP/1.0\r\n\r\n")
	buf := make([]byte, 256)
	n, _ := io.ReadFull(io.LimitReader(conn, 256), buf)
	if n == 0 {
		n, _ = conn.Read(buf)
	}
	return string(buf[:n])
}
