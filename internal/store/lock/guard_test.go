package lock

import (
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGuarder_Guard_RunsUnderLock verifies the guarded function runs and the
// lock is released afterward (a fresh Acquire succeeds immediately).
func TestGuarder_Guard_RunsUnderLock(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".mgit")
	g := NewGuarder(dir, DefaultTimeout)

	ran := false
	err := g.Guard(func() error { ran = true; return nil })
	require.NoError(t, err)
	assert.True(t, ran, "guarded fn did not run")

	// The lock was released, so it is immediately re-acquirable.
	lk, err := Acquire(dir, 0)
	require.NoError(t, err, "lock not released after Guard")
	_ = lk.Release()
}

// TestGuarder_Guard_PropagatesError verifies the guarded function's error is
// returned and the lock is still released on the error path.
func TestGuarder_Guard_PropagatesError(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".mgit")
	g := NewGuarder(dir, DefaultTimeout)

	sentinel := assert.AnError
	err := g.Guard(func() error { return sentinel })
	require.ErrorIs(t, err, sentinel)

	lk, err := Acquire(dir, 0)
	require.NoError(t, err, "lock not released after a failing Guard")
	_ = lk.Release()
}

// TestGuarder_Guard_AcquireFailure_DoesNotRunFn verifies that when the lock is
// held by someone else and cannot be acquired within the timeout, Guard returns
// the acquisition error and does NOT run fn. Refs: MGIT-46
func TestGuarder_Guard_AcquireFailure_DoesNotRunFn(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".mgit")
	held, err := Acquire(dir, DefaultTimeout)
	require.NoError(t, err)
	defer func() { _ = held.Release() }()

	g := NewGuarder(dir, 0) // zero timeout: fail immediately on contention
	ran := false
	err = g.Guard(func() error { ran = true; return nil })
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrLockHeld)
	assert.False(t, ran, "fn must not run when the lock cannot be acquired")
}

// TestHolderLabelFrom covers the label-formatting edge cases used in the
// contended-lock diagnostic: empty argv, base-naming + args, the length cap,
// and newline flattening (so the first-line-is-PID lockfile contract holds).
// Refs: MGIT-46
func TestHolderLabelFrom(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"empty", nil, ""},
		{"binary_only", []string{"/usr/local/bin/mgit"}, "mgit"},
		{"with_subcommand", []string{"/bin/mgit", "serve", "--mcp-only"}, "mgit serve --mcp-only"},
		{"newlines_flattened", []string{"mgit", "a\nb"}, "mgit a b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, holderLabelFrom(tt.args))
		})
	}

	t.Run("capped_at_120", func(t *testing.T) {
		long := make([]string, 40)
		for i := range long {
			long[i] = "arg"
		}
		got := holderLabelFrom(append([]string{"mgit"}, long...))
		assert.LessOrEqual(t, len(got), 120)
	})
}

// TestGuarder_Nil_IsPassthrough verifies a nil *Guarder runs the fn without
// locking — the "lock already held for my lifetime" case (e.g. a CLI command
// or a unit test with no locker wired).
func TestGuarder_Nil_IsPassthrough(t *testing.T) {
	var g *Guarder
	ran := false
	err := g.Guard(func() error { ran = true; return nil })
	require.NoError(t, err)
	assert.True(t, ran)
}

// TestGuarder_Guard_SerializesConcurrent proves concurrent Guards against the
// same dir never overlap — flock is per-open-file-description, so even
// same-process callers serialize. Run under -race. Refs: MGIT-46
func TestGuarder_Guard_SerializesConcurrent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".mgit")
	g := NewGuarder(dir, DefaultTimeout)

	var inside int32
	var maxSeen int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = g.Guard(func() error {
				n := atomic.AddInt32(&inside, 1)
				for {
					m := atomic.LoadInt32(&maxSeen)
					if n <= m || atomic.CompareAndSwapInt32(&maxSeen, m, n) {
						break
					}
				}
				time.Sleep(time.Millisecond)
				atomic.AddInt32(&inside, -1)
				return nil
			})
		}()
	}
	wg.Wait()
	assert.Equal(t, int32(1), maxSeen, "critical section entered concurrently")
}
