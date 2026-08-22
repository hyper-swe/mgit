package sandboxd

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The idle check asked the BACKEND whether anything was registered.
//
// The backend knows only BOOTED VMs. Registration is a service-layer fact, so
// with lazy provisioning — the documented default of `mgit work --sandbox` — a
// task that is registered but not yet used has no backend VM, the daemon
// reports itself idle, and it exits while a live task is bound to it.
//
// Measured before the fix: `mgit work --sandbox` with no exec, daemon observed
// alive, gone ~200s later with the registration still durable in the index.
//
// Same layering shape as MGIT-107's drain: the daemon reaching past the
// service to the backend and getting an answer that is true of the backend and
// false of the system. Refs: MGIT-154, MGIT-107, FR-17.9, FR-17.10
func TestDaemon_RegisteredButUnbootedSandbox_KeepsTheDaemonAlive(t *testing.T) {
	// The backend has NOTHING: no VM has booted. The service has a task.
	manager := newFakeManager()
	svc := newDrainRecorder("MGIT-154")

	cfg, _ := testConfig(t, manager)
	cfg.Service = svc
	cfg.IdleGrace = 60 * time.Millisecond
	cfg.PollInterval = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runDaemon(ctx, t, cfg)
	_ = waitForSocket(t, cfg.SocketPath)

	// Well past the idle grace: a daemon that consults the backend exits here.
	select {
	case err := <-done:
		t.Fatalf("the daemon exited while a registered task was bound to it (err=%v) — "+
			"lazy provisioning means a registered sandbox has no backend VM yet", err)
	case <-time.After(400 * time.Millisecond):
	}

	cancel()
	require.NoError(t, <-done)
}

// With nothing registered, the frugality NFR-17.6 intends is preserved: the
// daemon still exits on its idle grace. Refs: MGIT-154, NFR-17.6
func TestDaemon_NothingRegistered_StillExitsIdle(t *testing.T) {
	cfg, logs := testConfig(t, newFakeManager())
	cfg.Service = newDrainRecorder() // knows no tasks
	cfg.IdleGrace = 50 * time.Millisecond
	cfg.PollInterval = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runDaemon(ctx, t, cfg)
	_ = waitForSocket(t, cfg.SocketPath)

	select {
	case err := <-done:
		require.NoError(t, err)
		assert.Contains(t, logs.String(), `"idle_exit"`,
			"an idle daemon with nothing registered must still exit")
	case <-time.After(3 * time.Second):
		t.Fatal("the daemon never exited idle; frugality was lost")
	}
}

// A build with no wired service falls back to the backend, exactly as before.
// Refs: MGIT-154, MGIT-11.10.8
func TestDaemon_WithoutAService_IdleCheckStillUsesTheBackend(t *testing.T) {
	cfg, _ := testConfig(t, newFakeManager("01JXSB1"))
	cfg.Service = nil
	cfg.IdleGrace = 50 * time.Millisecond
	cfg.PollInterval = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runDaemon(ctx, t, cfg)
	_ = waitForSocket(t, cfg.SocketPath)

	select {
	case err := <-done:
		t.Fatalf("a service-less daemon with a live backend sandbox exited idle: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	cancel()
	require.NoError(t, <-done)
}
