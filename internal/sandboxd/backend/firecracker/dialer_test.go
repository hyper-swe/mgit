package firecracker

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/sandboxd/backend/microvm"
)

// shortDir returns a short temp dir; macOS unix-socket paths are limited
// to ~104 bytes, so t.TempDir() (a long path) cannot host a socket there.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "fcd")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// fakeGuestVsock listens at the firecracker vsock socket path a sandbox
// would use and answers the firecracker CONNECT handshake with "OK 0\n".
// It records the CONNECT line it received.
func fakeGuestVsock(t *testing.T, socketPath string) chan string {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(socketPath), 0o700))
	ln, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	got := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		buf := make([]byte, 64)
		n, _ := conn.Read(buf)
		got <- string(buf[:n])
		_, _ = io.WriteString(conn, "OK 0\n")
	}()
	return got
}

// TestGuestDialer_PathMatchesBackendConvention pins the path convention:
// the dialer's vsock socket for a sandbox must equal the firecracker
// per-VM layout under microvm's <workDir>/<id> state dir. If either the
// state-dir convention or the socket name drifts, this fails. Refs: FR-17.11
func TestGuestDialer_PathMatchesBackendConvention(t *testing.T) {
	workDir := t.TempDir()
	d := newGuestDialer(workDir)
	// Assert against microvm.SandboxStateDir (the manager's own convention)
	// so a drift in the manager's per-sandbox layout breaks this test.
	want := sandboxPaths(microvm.SandboxStateDir(workDir, "sbx-123")).vsock
	assert.Equal(t, want, d.vsockSocketPath("sbx-123"))
	assert.Equal(t, filepath.Join(workDir, "sbx-123", "vsock.sock"), d.vsockSocketPath("sbx-123"))
}

// TestGuestDialer_DialsGuestPort verifies DialGuest connects to the
// sandbox's vsock socket and requests the guest vsock port via the
// firecracker handshake. Refs: FR-17.11
func TestGuestDialer_DialsGuestPort(t *testing.T) {
	workDir := shortDir(t)
	d := newGuestDialer(workDir)
	got := fakeGuestVsock(t, d.vsockSocketPath("sbx-1"))

	conn, err := d.DialGuest(context.Background(), "sbx-1")
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()

	assert.Equal(t, "CONNECT 1024\n", <-got, "the dialer requests the guest vsock port")
}

// TestLandStreamOpener_DialsGuestLandPort verifies the host land dialer
// connects to the sandbox's vsock socket and requests the guest LAND port
// (distinct from the exec port 1024), so the host pulls the object pool
// over the dedicated land channel. Refs: FR-17.5
func TestLandStreamOpener_DialsGuestLandPort(t *testing.T) {
	workDir := shortDir(t)
	d := NewLandDialer(workDir)
	// The land dialer uses the same per-VM vsock socket as exec.
	got := fakeGuestVsock(t, sandboxPaths(microvm.SandboxStateDir(workDir, "sbx-1")).vsock)

	conn, err := d.DialGuest(context.Background(), "sbx-1")
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()

	assert.Equal(t, "CONNECT 1025\n", <-got, "the land dialer requests the guest land port")
}

// TestGuestDialer_SandboxSocketAbsent verifies dialing a sandbox whose VM
// is not running (no vsock socket) fails closed.
func TestGuestDialer_SandboxSocketAbsent(t *testing.T) {
	d := newGuestDialer(shortDir(t))
	conn, err := d.DialGuest(context.Background(), "never-launched")
	require.Error(t, err)
	assert.Nil(t, conn)
}
