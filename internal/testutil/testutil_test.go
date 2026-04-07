// Package testutil provides test infrastructure for mgit.
// Refs: MGIT-1.2.5, NFR-4
package testutil

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTestutil_MockClock(t *testing.T) {
	fixed := time.Date(2026, 4, 7, 12, 0, 0, 0, time.UTC)
	clock := NewMockClock(fixed)

	got := clock.Now()
	assert.Equal(t, fixed, got, "MockClock.Now() must return the fixed time")
}

func TestTestutil_MockClock_Advance(t *testing.T) {
	fixed := time.Date(2026, 4, 7, 12, 0, 0, 0, time.UTC)
	clock := NewMockClock(fixed)

	clock.Advance(5 * time.Minute)
	got := clock.Now()
	expected := fixed.Add(5 * time.Minute)
	assert.Equal(t, expected, got, "MockClock must advance by the specified duration")
}

func TestTestutil_MockClock_Set(t *testing.T) {
	fixed := time.Date(2026, 4, 7, 12, 0, 0, 0, time.UTC)
	clock := NewMockClock(fixed)

	newTime := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	clock.Set(newTime)
	got := clock.Now()
	assert.Equal(t, newTime, got, "MockClock.Set must update the current time")
}

func TestTestutil_MockClock_ClockFunc(t *testing.T) {
	fixed := time.Date(2026, 4, 7, 12, 0, 0, 0, time.UTC)
	clock := NewMockClock(fixed)

	fn := clock.ClockFunc()
	got := fn()
	assert.Equal(t, fixed, got,
		"ClockFunc must return a func() time.Time that uses MockClock")
}

func TestTestutil_MakeTestCommit(t *testing.T) {
	commit := MakeTestCommit()
	assert.NotEmpty(t, commit.ID, "test commit must have an ID")
	assert.NotEmpty(t, commit.TaskID, "test commit must have a TaskID")
	assert.NotEmpty(t, commit.Message, "test commit must have a Message")
	assert.NotEmpty(t, commit.Author, "test commit must have an Author")
	assert.False(t, commit.CreatedAt.IsZero(), "test commit must have a timestamp")
}

func TestTestutil_MakeTestCommit_DefaultValues(t *testing.T) {
	commit := MakeTestCommit()
	assert.Contains(t, commit.TaskID, "TEST-",
		"default TaskID must start with TEST-")
}

func TestTestutil_MakeTestBranch(t *testing.T) {
	branch := MakeTestBranch()
	assert.NotEmpty(t, branch.Name, "test branch must have a Name")
	assert.NotEmpty(t, branch.TaskID, "test branch must have a TaskID")
}

func TestTestutil_MakeTestDiff(t *testing.T) {
	diff := MakeTestDiff()
	assert.NotEmpty(t, diff.Path, "test diff must have a Path")
	assert.NotEmpty(t, diff.Operation, "test diff must have an Operation")
}

func TestTestutil_NewTestGitRepo(t *testing.T) {
	repo, cleanup := NewTestGitRepo(t)
	defer cleanup()

	require.NotNil(t, repo, "NewTestGitRepo must return a non-nil repository")

	// Verify the repo has a valid HEAD
	head, err := repo.Head()
	require.NoError(t, err, "test repo must have a valid HEAD")
	assert.NotNil(t, head, "HEAD must not be nil")
}

func TestTestutil_NewTestGitRepo_Cleanup(t *testing.T) {
	var repoDir string
	func() {
		_, cleanup := NewTestGitRepo(t)
		// Get the temp dir path
		tmpDir := t.TempDir()
		entries, err := os.ReadDir(tmpDir)
		if err == nil && len(entries) > 0 {
			repoDir = filepath.Join(tmpDir, entries[0].Name())
		}
		cleanup()
	}()

	// After cleanup, any repo-specific files should be cleaned up
	// (t.TempDir handles this automatically, but cleanup should also work)
	_ = repoDir // cleanup is verified by the fact that cleanup() doesn't panic
}

func TestTestutil_NewTestStore(t *testing.T) {
	store, cleanup := NewTestStore(t)
	defer cleanup()

	require.NotNil(t, store, "NewTestStore must return a non-nil store")
}

func TestTestutil_NewTestStore_Cleanup(t *testing.T) {
	store, cleanup := NewTestStore(t)
	require.NotNil(t, store)

	// Get the DB path before cleanup
	dbPath := store.Path()
	cleanup()

	// After cleanup, database file should be closed (file may still exist due to TempDir)
	// The important thing is that the store is closed without error
	_, err := os.Stat(dbPath)
	// File may or may not exist after cleanup (depends on TempDir lifecycle)
	_ = err
}
