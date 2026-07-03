package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gitstore "github.com/hyper-swe/mgit/internal/store/git"
	"github.com/hyper-swe/mgit/internal/store/lock"
)

func testClock() func() time.Time {
	return func() time.Time { return time.Now().UTC() }
}

// openInitedApp inits a fresh repo and opens an App holding its lifetime lock.
func openInitedApp(t *testing.T) (*App, string) {
	t.Helper()
	dir := t.TempDir()
	_, err := gitstore.Init(dir, testClock())
	require.NoError(t, err)
	app, err := OpenApp(dir)
	require.NoError(t, err)
	t.Cleanup(app.Close)
	return app, filepath.Join(dir, ".mgit")
}

// TestApp_DetachLock_ReleasesLifetimeLock is the core MGIT-46 property: while
// the App holds the lifetime lock the CLI cannot acquire it, but after
// DetachLock (what `mgit serve` does) the lock is immediately free — no 30s
// starvation — and the returned Guarder still guards operations. Refs: MGIT-46
func TestApp_DetachLock_ReleasesLifetimeLock(t *testing.T) {
	app, storeDir := openInitedApp(t)

	// Precondition: the lifetime lock is held, so a non-blocking acquire fails.
	_, err := lock.Acquire(storeDir, 0)
	require.Error(t, err, "App should hold the lifetime lock before DetachLock")

	locker := app.DetachLock()
	require.NotNil(t, locker)

	// The CLI can now acquire immediately — the starvation bug is gone.
	lk, err := lock.Acquire(storeDir, 0)
	require.NoError(t, err, "after DetachLock the CLI must acquire immediately")
	require.NoError(t, lk.Release())

	// The detached locker still serializes server operations per-op.
	ran := false
	require.NoError(t, locker.Guard(func() error { ran = true; return nil }))
	assert.True(t, ran)
}

// TestServe_LockCoexistsWithCLI_NoStarvation models the reported failure: a
// long-lived server (per-operation guarding via the detached locker) and a
// CLI hammering the same repo both make progress and finish well within the
// timeout, instead of the CLI hanging 30s per call. Run under -race.
// Refs: MGIT-46
func TestServe_LockCoexistsWithCLI_NoStarvation(t *testing.T) {
	app, storeDir := openInitedApp(t)
	locker := app.DetachLock() // serve side: per-operation lock

	const rounds = 25
	serveErr := make(chan error, 1)
	go func() {
		for i := 0; i < rounds; i++ {
			if err := locker.Guard(func() error { return nil }); err != nil {
				serveErr <- err
				return
			}
		}
		serveErr <- nil
	}()

	// CLI side: acquire per "command". None may hit the 30s starvation timeout.
	cliDone := make(chan error, 1)
	go func() {
		for i := 0; i < rounds; i++ {
			lk, err := lock.Acquire(storeDir, 5*time.Second)
			if err != nil {
				cliDone <- err
				return
			}
			if err := lk.Release(); err != nil {
				cliDone <- err
				return
			}
		}
		cliDone <- nil
	}()

	deadline := time.After(15 * time.Second)
	for got := 0; got < 2; got++ {
		select {
		case err := <-serveErr:
			require.NoError(t, err, "serve-side guarded op failed")
		case err := <-cliDone:
			require.NoError(t, err, "CLI-side acquire failed (starvation?)")
		case <-deadline:
			t.Fatal("serve+CLI did not both finish — starvation or deadlock")
		}
	}
}
