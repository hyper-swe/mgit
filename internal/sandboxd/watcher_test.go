package sandboxd

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type countingWatcher struct {
	mu     sync.Mutex
	calls  int
	err    error
	panics bool
}

func (w *countingWatcher) Observe(context.Context) error {
	w.mu.Lock()
	w.calls++
	shouldPanic, err := w.panics, w.err
	w.mu.Unlock()
	if shouldPanic {
		panic("induced panic in the worktree watcher")
	}
	return err
}

func (w *countingWatcher) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.calls
}

// The passive watcher must actually be driven by the daemon — that is the
// whole difference between "snapshots exist as a verb" and "recovery does not
// depend on the agent's virtue". Refs: MGIT-110, R-H234
func TestDaemon_DrivesTheWorktreeWatcherOnItsOwnCadence(t *testing.T) {
	watcher := &countingWatcher{}
	cfg, _ := testConfig(t, newFakeManager("01JXSB1"))
	cfg.IdleGrace = time.Hour
	cfg.Watcher = watcher
	cfg.SnapshotInterval = 20 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := runDaemon(ctx, t, cfg)
	_ = waitForSocket(t, cfg.SocketPath)

	require.Eventually(t, func() bool { return watcher.count() >= 2 },
		2*time.Second, 10*time.Millisecond, "the daemon must tick the watcher without being asked")

	cancel()
	require.NoError(t, <-done)
}

// A watcher failure is housekeeping, not supervision: it must be reported and
// must never stop the daemon that is holding VMs. Refs: MGIT-110
func TestDaemon_WatcherFailure_IsLoggedAndTheDaemonKeepsSupervising(t *testing.T) {
	watcher := &countingWatcher{err: errors.New("worktree unreadable")}
	cfg, logs := testConfig(t, newFakeManager("01JXSB1"))
	cfg.IdleGrace = time.Hour
	cfg.Watcher = watcher
	cfg.SnapshotInterval = 20 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := runDaemon(ctx, t, cfg)
	_ = waitForSocket(t, cfg.SocketPath)

	require.Eventually(t, func() bool { return watcher.count() >= 2 },
		2*time.Second, 10*time.Millisecond)
	assert.Contains(t, logs.String(), `"snapshot_error"`)

	cancel()
	require.NoError(t, <-done, "a failing watcher must not take the daemon down")
}

// Same law as the drain (MGIT-107): a panic in housekeeping must not kill the
// process that supervises microVMs. Refs: MGIT-110, MGIT-107
func TestDaemon_WatcherPanic_DoesNotKillTheDaemon(t *testing.T) {
	watcher := &countingWatcher{panics: true}
	cfg, logs := testConfig(t, newFakeManager("01JXSB1"))
	cfg.IdleGrace = time.Hour
	cfg.Watcher = watcher
	cfg.SnapshotInterval = 20 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := runDaemon(ctx, t, cfg)
	_ = waitForSocket(t, cfg.SocketPath)

	require.Eventually(t, func() bool { return watcher.count() >= 2 },
		2*time.Second, 10*time.Millisecond, "the watcher keeps being driven after a panic")
	assert.Contains(t, logs.String(), `"snapshot_error"`)

	cancel()
	require.NoError(t, <-done)
}

// No watcher wired is the honest default: the daemon runs exactly as before.
func TestDaemon_WithoutAWatcher_RunsUnchanged(t *testing.T) {
	cfg, _ := testConfig(t, newFakeManager("01JXSB1"))
	cfg.IdleGrace = time.Hour
	cfg.Watcher = nil

	ctx, cancel := context.WithCancel(context.Background())
	done := runDaemon(ctx, t, cfg)
	_ = waitForSocket(t, cfg.SocketPath)
	cancel()
	require.NoError(t, <-done)
}
