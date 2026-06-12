// Package sandboxd tests verify authenticated IPC per MGIT-11.4.2
// acceptance criteria (F-08, ASVS V4). Refs: FR-17.34
package sandboxd

import (
	"bufio"
	"context"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// dialAndRead connects and reads the daemon's auth verdict line (empty
// on connection close without a greeting).
func dialAndRead(t *testing.T, socketPath string) string {
	t.Helper()
	conn := waitForSocket(t, socketPath)
	defer func() { _ = conn.Close() }()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(2*time.Second)))
	line, _ := bufio.NewReader(conn).ReadString('\n')
	return line
}

// TestIPC_SameUID_Accepted verifies a same-UID client passes peer
// authentication and receives the daemon greeting. Refs: FR-17.34
func TestIPC_SameUID_Accepted(t *testing.T) {
	manager := newFakeManager("01JXSB1")
	cfg, _ := testConfig(t, manager)
	cfg.IdleGrace = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := runDaemon(ctx, t, cfg)

	line := dialAndRead(t, cfg.SocketPath)
	assert.True(t, strings.HasPrefix(line, "ok"),
		"a same-UID peer (this test process) must be accepted, got %q", line)

	cancel()
	require.NoError(t, <-done)
}

// TestIPC_DifferentUID_Rejected verifies a foreign-UID peer is refused
// before any control handling, and the rejection is audited.
// Refs: FR-17.34
func TestIPC_DifferentUID_Rejected(t *testing.T) {
	manager := newFakeManager("01JXSB1")
	cfg, logs := testConfig(t, manager)
	cfg.IdleGrace = time.Hour
	// Inject a credential reader reporting a foreign UID: a real
	// cross-UID connection needs root, so the seam is the lookup, not
	// the socket.
	cfg.PeerUID = func(*net.UnixConn) (uint32, error) {
		return uint32(os.Getuid()) + 1, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := runDaemon(ctx, t, cfg)

	line := dialAndRead(t, cfg.SocketPath)
	assert.False(t, strings.HasPrefix(line, "ok"),
		"a foreign-UID peer must never receive the greeting")

	require.Eventually(t, func() bool {
		return strings.Contains(logs.String(), `"auth_rejected"`)
	}, 2*time.Second, 10*time.Millisecond, "the rejection must be audited")

	cancel()
	require.NoError(t, <-done)

	t.Run("credential_lookup_failure_rejected", func(t *testing.T) {
		cfg2, logs2 := testConfig(t, manager)
		cfg2.IdleGrace = time.Hour
		cfg2.PeerUID = func(*net.UnixConn) (uint32, error) {
			return 0, assert.AnError
		}
		ctx2, cancel2 := context.WithCancel(context.Background())
		defer cancel2()
		done2 := runDaemon(ctx2, t, cfg2)

		line := dialAndRead(t, cfg2.SocketPath)
		assert.False(t, strings.HasPrefix(line, "ok"),
			"unverifiable peers fail closed")
		require.Eventually(t, func() bool {
			return strings.Contains(logs2.String(), `"auth_rejected"`)
		}, 2*time.Second, 10*time.Millisecond)

		cancel2()
		require.NoError(t, <-done2)
	})
}

// TestIPC_UnauthenticatedPeers_CannotKeepDaemonAlive verifies the
// NFR-17.6 lifecycle is auth-gated: a foreign-UID dialer hammering the
// socket must not reset the idle clock — unauthorized peers do not
// control daemon lifetime. Refs: FR-17.34, NFR-17.6
func TestIPC_UnauthenticatedPeers_CannotKeepDaemonAlive(t *testing.T) {
	manager := newFakeManager() // zero sandboxes => idle-exit eligible
	cfg, _ := testConfig(t, manager)
	cfg.PeerUID = func(*net.UnixConn) (uint32, error) {
		return uint32(os.Getuid()) + 1, nil // every peer is foreign
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := runDaemon(ctx, t, cfg)
	_ = waitForSocket(t, cfg.SocketPath).Close()

	// Hammer the socket more often than the idle grace period.
	hammerDone := make(chan struct{})
	go func() {
		defer close(hammerDone)
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			if conn, err := net.Dial("unix", cfg.SocketPath); err == nil {
				_ = conn.Close()
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()

	select {
	case err := <-done:
		require.NoError(t, err, "the daemon must idle-exit despite unauthenticated dials")
	case <-time.After(3 * time.Second):
		t.Fatal("unauthenticated dials kept the daemon alive (NFR-17.6 violated)")
	}
	cancel()
	<-hammerDone
}

// TestIPC_NonUnixPeer_Rejected covers the defensive branch: a
// connection that is not a unix socket can never authenticate.
// Refs: FR-17.34
func TestIPC_NonUnixPeer_Rejected(t *testing.T) {
	cfg, logs := testConfig(t, newFakeManager())
	d, err := New(cfg)
	require.NoError(t, err)

	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	defer func() { _ = server.Close() }()

	assert.False(t, d.authenticate(server), "net.Pipe conns carry no kernel credentials")
	assert.Contains(t, logs.String(), `"auth_rejected"`)
}

// TestIPC_SocketPermissions_0600 verifies the socket file is
// owner-only. Refs: FR-17.34
func TestIPC_SocketPermissions_0600(t *testing.T) {
	manager := newFakeManager("01JXSB1")
	cfg, _ := testConfig(t, manager)
	cfg.IdleGrace = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := runDaemon(ctx, t, cfg)
	_ = waitForSocket(t, cfg.SocketPath).Close()

	info, err := os.Stat(cfg.SocketPath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(),
		"socket must be owner-only (F-08)")

	cancel()
	require.NoError(t, <-done)
}
