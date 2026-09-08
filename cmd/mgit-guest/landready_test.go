package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func seedStore(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), ".mgit")
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "refs", "heads", "task"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "HEAD"), []byte("ref: refs/heads/task/MGIT-199\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "refs", "heads", "task", "MGIT-199"), []byte("aaaa\n"), 0o600))
	return dir
}

// The land-ready notify fires only when the guest's private store CHANGED
// across an exec — a boot's setup execs commit nothing, so they must not
// each cost the host a land pass (three "notify-triggered land completed,
// commits:0" within 70 ms of a boot, MGIT-199). Refs: MGIT-199, MGIT-11.10.11
func TestLandReadyGate_UnchangedStore_NeverSignals(t *testing.T) {
	gate := newLandReadyGate(seedStore(t))
	for i := 0; i < 3; i++ {
		assert.False(t, gate.changed(), "exec %d changed nothing, so nothing to land", i+1)
	}
}

func TestLandReadyGate_RefMoved_SignalsOnce(t *testing.T) {
	dir := seedStore(t)
	gate := newLandReadyGate(dir)
	require.False(t, gate.changed())
	require.NoError(t, os.WriteFile(filepath.Join(dir, "refs", "heads", "task", "MGIT-199"), []byte("bbbb\n"), 0o600))
	assert.True(t, gate.changed(), "a commit moved the branch: signal")
	assert.False(t, gate.changed(), "and only once for that commit")
}

func TestLandReadyGate_NewBranchOrHeadChange_Signals(t *testing.T) {
	dir := seedStore(t)
	gate := newLandReadyGate(dir)
	require.False(t, gate.changed())
	require.NoError(t, os.WriteFile(filepath.Join(dir, "refs", "heads", "task", "MGIT-199.1"), []byte("cccc\n"), 0o600))
	assert.True(t, gate.changed(), "a new ref is a change")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "HEAD"), []byte("ref: refs/heads/task/MGIT-199.1\n"), 0o600))
	assert.True(t, gate.changed(), "HEAD moving is a change")
}

// A sandbox with no store (no worktree delivered) has nothing to land, ever.
func TestLandReadyGate_AbsentStore_NeverSignals(t *testing.T) {
	gate := newLandReadyGate(filepath.Join(t.TempDir(), "nope", ".mgit"))
	assert.False(t, gate.changed())
	assert.False(t, gate.changed())
}
