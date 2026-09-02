package sandboxd

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sleepingWatcher holds each pass for a fixed duration and IGNORES its
// context — like the production walk, which hashes every file of every
// supervised worktree and takes no context at all. A cancellable stand-in
// passes against the broken loop (measured by the campaign at 507us), so
// this one deliberately cannot be shortened. Refs: MGIT-170
type sleepingWatcher struct {
	pass    time.Duration
	started chan struct{} // one send per pass, non-blocking

	mu    sync.Mutex
	calls int
}

func (w *sleepingWatcher) Observe(context.Context) error {
	w.mu.Lock()
	w.calls++
	w.mu.Unlock()
	select {
	case w.started <- struct{}{}:
	default:
	}
	time.Sleep(w.pass) // no select on ctx.Done(): the real walk has none
	return nil
}

func (w *sleepingWatcher) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.calls
}

// awaitPassStart blocks until the watcher has begun a pass.
func awaitPassStart(t *testing.T, w *sleepingWatcher) {
	t.Helper()
	select {
	case <-w.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the daemon never drove a snapshot pass")
	}
}

// readGreeting dials the daemon and returns how long the FIRST BYTE of the
// greeting took to arrive. The dial itself always succeeds at once — the
// kernel accepts into the listener backlog — so the dial is not the
// measurement; the greeting is, because nothing is written until the select
// loop hands the connection to a handler. Refs: MGIT-170
func readGreeting(t *testing.T, socketPath string) time.Duration {
	t.Helper()
	conn, err := net.Dial("unix", socketPath)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(10*time.Second)))
	start := time.Now()
	buf := make([]byte, len(greeting))
	_, err = io.ReadFull(conn, buf)
	elapsed := time.Since(start)
	require.NoError(t, err, "the greeting must arrive")
	require.Equal(t, greeting, string(buf))
	return elapsed
}

// A client must be greeted promptly while a snapshot pass is in flight.
//
// The pass used to run INLINE in Run's select, so every connection accepted
// during one waited for the whole remaining pass before a handler saw it —
// and a pass is O(worktree) per supervised task, the one cost that scales
// with the user's repository rather than with mgit. The request path must
// not wait on housekeeping. Refs: MGIT-170
func TestDaemon_AClientIsGreetedWhileASnapshotPassIsInFlight(t *testing.T) {
	const pass = 1500 * time.Millisecond
	watcher := &sleepingWatcher{pass: pass, started: make(chan struct{}, 1)}
	cfg, _ := testConfig(t, newFakeManager("01JXSB1"))
	cfg.IdleGrace = time.Hour
	cfg.Watcher = watcher
	cfg.SnapshotInterval = 20 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := runDaemon(ctx, t, cfg)
	_ = waitForSocket(t, cfg.SocketPath)
	awaitPassStart(t, watcher)

	elapsed := readGreeting(t, cfg.SocketPath)

	t.Logf("first byte of the greeting after %v, with a %v pass in flight", elapsed, pass)
	assert.Less(t, elapsed, 250*time.Millisecond,
		"a client must not wait for the snapshot pass: first byte took %v of a %v pass", elapsed, pass)

	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the daemon did not stop")
	}
}

// Shutdown must not wait for an uninterruptible pass.
//
// Drain is what writes the terminal record and reaps the VMs; a daemon that
// is SIGKILLed by its supervisor before reaching it leaves running VMs
// unsupervised, and the next daemon stamps them killed/unsupervised — the
// record MGIT-107 was closed to stop manufacturing. With the pass inline, a
// shutdown waited for the whole pass; and when a pass outlasts the tick, the
// select re-enters with BOTH ctx.Done and the ticker ready, so each iteration
// was a coin flip between shutting down and starting another pass (measured:
// 7.9s against a 2s pass). Refs: MGIT-170, MGIT-107
func TestDaemon_ShutdownDoesNotWaitForAnUninterruptiblePass(t *testing.T) {
	const pass = 2 * time.Second
	watcher := &sleepingWatcher{pass: pass, started: make(chan struct{}, 1)}
	manager := newFakeManager("01JXSB1")
	cfg, logs := testConfig(t, manager)
	cfg.IdleGrace = time.Hour
	cfg.Watcher = watcher
	cfg.SnapshotInterval = 20 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := runDaemon(ctx, t, cfg)
	_ = waitForSocket(t, cfg.SocketPath)
	awaitPassStart(t, watcher)

	start := time.Now()
	cancel()
	var err error
	select {
	case err = <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the daemon did not stop")
	}
	elapsed := time.Since(start)

	t.Logf("shutdown returned after %v, with a %v pass in flight", elapsed, pass)
	require.NoError(t, err)
	assert.Less(t, elapsed, 1*time.Second,
		"shutdown must complete within the drain budget, not after the pass: took %v of a %v pass", elapsed, pass)
	assert.Contains(t, logs.String(), `"drained"`, "the drain still runs and writes its record")
	assert.Equal(t, 1, watcher.count(), "no second pass was started on the way out")
}

// A pass that outlasts the tick interval is skipped, not queued: however many
// ticks fire while one is running, exactly one pass is in flight, and the
// cadence resumes afterwards. This is the property the fix rests on; it is a
// guard against a future refactor that queues passes, and it does NOT
// distinguish the inline loop (which also never ran two at once — it ran
// nothing else either). Refs: MGIT-170
func TestDaemon_SnapshotPasses_AreSingleFlight(t *testing.T) {
	const pass = 300 * time.Millisecond
	watcher := &sleepingWatcher{pass: pass, started: make(chan struct{}, 1)}
	cfg, _ := testConfig(t, newFakeManager("01JXSB1"))
	cfg.IdleGrace = time.Hour
	cfg.Watcher = watcher
	cfg.SnapshotInterval = 20 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := runDaemon(ctx, t, cfg)
	_ = waitForSocket(t, cfg.SocketPath)
	awaitPassStart(t, watcher)

	time.Sleep(pass / 2) // ~7 ticks fire while the first pass is still running
	assert.Equal(t, 1, watcher.count(), "ticks during a pass must not start another")
	require.Eventually(t, func() bool { return watcher.count() >= 2 },
		2*time.Second, 10*time.Millisecond, "the cadence resumes once the pass ends")

	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the daemon did not stop")
	}
}
