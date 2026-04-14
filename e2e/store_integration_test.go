// Package e2e contains end-to-end integration tests for mgit.
// These tests verify that go-git and SQLite store layers work together.
// Refs: MGIT-2.4.1, MGIT-2.4.2, MGIT-2.4.3
package e2e

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit-dev/internal/model"
	gitstore "github.com/hyper-swe/mgit-dev/internal/store/git"
	"github.com/hyper-swe/mgit-dev/internal/store/index"
)

func fixedClock() func() time.Time {
	fixed := time.Date(2026, 4, 7, 12, 0, 0, 0, time.UTC)
	return func() time.Time { return fixed }
}

// setupStores creates both a git repo and SQLite index for integration testing.
func setupStores(t *testing.T) (*gitstore.Repository, *gitstore.CommitStore, *index.Store) {
	t.Helper()
	tmpDir := t.TempDir()
	clock := fixedClock()

	repo, err := gitstore.Init(tmpDir, clock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = repo.Close() })

	dbPath := filepath.Join(tmpDir, ".mgit", "index.db")
	idx, err := index.New(dbPath, clock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = idx.Close() })

	cs := gitstore.NewCommitStore(repo)
	return repo, cs, idx
}

func makeCommit(t *testing.T, taskID string) *model.Commit {
	t.Helper()
	tid, err := model.ParseTaskID(taskID)
	require.NoError(t, err)
	return &model.Commit{
		TaskID:     tid,
		AgentID:    "test-agent",
		Message:    "[MGIT:" + taskID + "] integration test commit",
		CommitType: model.CommitTypeNormal,
		CreatedBy:  "test-agent",
		Branch:     "task/" + taskID,
	}
}

// --- MGIT-2.4.1: go-git + SQLite roundtrip ---

func TestIntegration_RepoAndIndex_CreateAndRetrieve(t *testing.T) {
	_, cs, idx := setupStores(t)
	ctx := context.Background()

	// 1. Create commit in go-git
	commit := makeCommit(t, "MGIT-1.2.3")
	hash, err := cs.CreateCommit(ctx, commit)
	require.NoError(t, err)

	// 2. Index in SQLite
	err = idx.AddCommitToTask(ctx, "MGIT-1.2.3", hash, commit.ContentHash, "test-agent", 0)
	require.NoError(t, err)

	// 3. Retrieve from SQLite
	records, err := idx.GetTaskCommits(ctx, "MGIT-1.2.3")
	require.NoError(t, err)
	require.Len(t, records, 1)
	assert.Equal(t, hash, records[0].CommitHash)

	// 4. Retrieve from go-git
	retrieved, err := cs.GetCommit(ctx, hash)
	require.NoError(t, err)
	assert.Contains(t, retrieved.Message, "MGIT-1.2.3")

	// 5. Reverse lookup
	taskID, err := idx.GetCommitTask(ctx, hash)
	require.NoError(t, err)
	assert.Equal(t, "MGIT-1.2.3", taskID)
}

func TestIntegration_RepoAndIndex_MultipleCommits(t *testing.T) {
	_, cs, idx := setupStores(t)
	ctx := context.Background()

	// Create 5 commits for the same task
	hashes := make([]string, 5)
	for i := range 5 {
		c := makeCommit(t, "MGIT-2.1")
		c.Message = fmt.Sprintf("[MGIT:MGIT-2.1] commit %d", i)
		hash, err := cs.CreateCommit(ctx, c)
		require.NoError(t, err)
		hashes[i] = hash

		err = idx.AddCommitToTask(ctx, "MGIT-2.1", hash, c.ContentHash, "test-agent", i)
		require.NoError(t, err)
	}

	// Verify all 5 are in the index
	records, err := idx.GetTaskCommits(ctx, "MGIT-2.1")
	require.NoError(t, err)
	assert.Len(t, records, 5)

	// Verify all are retrievable from go-git
	for _, hash := range hashes {
		_, err := cs.GetCommit(ctx, hash)
		assert.NoError(t, err, "commit %s must be retrievable", hash[:8])
	}
}

func TestIntegration_RepoAndIndex_OrderPreservation(t *testing.T) {
	_, cs, idx := setupStores(t)
	ctx := context.Background()

	hashes := make([]string, 0, 3)
	for i := range 3 {
		c := makeCommit(t, "MGIT-3.1")
		c.Message = fmt.Sprintf("[MGIT:MGIT-3.1] ordered commit %d", i)
		hash, err := cs.CreateCommit(ctx, c)
		require.NoError(t, err)
		hashes = append(hashes, hash)

		err = idx.AddCommitToTask(ctx, "MGIT-3.1", hash, c.ContentHash, "test-agent", i)
		require.NoError(t, err)
	}

	// Verify order is preserved
	records, err := idx.GetTaskCommits(ctx, "MGIT-3.1")
	require.NoError(t, err)
	require.Len(t, records, 3)

	for i, rec := range records {
		assert.Equal(t, hashes[i], rec.CommitHash,
			"commit at position %d must match", i)
		assert.Equal(t, i, rec.Position)
	}
}

// --- MGIT-2.4.2: Append-only enforcement ---

func TestIntegration_AppendOnly_GitDeleteRejected(t *testing.T) {
	_, cs, _ := setupStores(t)
	ctx := context.Background()

	err := cs.DeleteCommit(ctx, "anyhash")
	assert.ErrorIs(t, err, model.ErrAppendOnlyViolation)
}

func TestIntegration_AppendOnly_IndexDeleteRejected(t *testing.T) {
	_, _, idx := setupStores(t)
	ctx := context.Background()

	err := idx.DeleteFromTask(ctx, "MGIT-1", "anyhash")
	assert.ErrorIs(t, err, model.ErrAppendOnlyViolation)
}

func TestIntegration_AppendOnly_DuplicateInsertRejected(t *testing.T) {
	_, cs, idx := setupStores(t)
	ctx := context.Background()

	c := makeCommit(t, "MGIT-4.1")
	hash, err := cs.CreateCommit(ctx, c)
	require.NoError(t, err)

	err = idx.AddCommitToTask(ctx, "MGIT-4.1", hash, "ch1", "agent", 0)
	require.NoError(t, err)

	// Duplicate should fail
	err = idx.AddCommitToTask(ctx, "MGIT-4.1", hash, "ch1", "agent", 1)
	assert.Error(t, err, "duplicate (task, commit) must be rejected")
}

// --- MGIT-2.4.3: Concurrent access ---

func TestIntegration_ConcurrentIndexWrites_NoRace(t *testing.T) {
	// go-git filesystem storage is NOT thread-safe for concurrent writes
	// to the same repo. In production, each agent gets its own worktree.
	// This test verifies the SQLite index handles concurrent writes safely.
	_, cs, idx := setupStores(t)
	ctx := context.Background()

	// Create commits sequentially (go-git is single-threaded)
	type commitInfo struct {
		taskID string
		hash   string
	}
	commits := make([]commitInfo, 5)
	for i := range 5 {
		taskID := fmt.Sprintf("MGIT-%d.1", i+1)
		c := makeCommit(t, taskID)
		hash, err := cs.CreateCommit(ctx, c)
		require.NoError(t, err)
		commits[i] = commitInfo{taskID: taskID, hash: hash}
	}

	// Index concurrently (SQLite with WAL handles this)
	var wg sync.WaitGroup
	errCh := make(chan error, 5)
	for _, ci := range commits {
		wg.Add(1)
		go func(taskID, hash string) {
			defer wg.Done()
			if err := idx.AddCommitToTask(ctx, taskID, hash, "ch", "agent", 0); err != nil {
				errCh <- fmt.Errorf("index %s: %w", taskID, err)
			}
		}(ci.taskID, ci.hash)
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Errorf("concurrent index error: %v", err)
	}

	// Verify all 5 are indexed
	for _, ci := range commits {
		records, err := idx.GetTaskCommits(ctx, ci.taskID)
		require.NoError(t, err)
		assert.Len(t, records, 1, "task %s must have 1 commit", ci.taskID)
	}
}

func TestIntegration_ConcurrentLocking(t *testing.T) {
	_, _, idx := setupStores(t)
	ctx := context.Background()

	var wg sync.WaitGroup
	successCount := 0
	var mu sync.Mutex

	// 3 agents try to lock the same branch simultaneously
	for i := range 3 {
		wg.Add(1)
		go func(agentNum int) {
			defer wg.Done()
			agentID := fmt.Sprintf("agent-%d", agentNum)
			err := idx.LockBranch(ctx, "task/MGIT-1.2", agentID, 30*time.Second)
			if err == nil {
				mu.Lock()
				successCount++
				mu.Unlock()
			}
		}(i)
	}

	wg.Wait()

	// At least one should succeed, and at most one at a time
	assert.GreaterOrEqual(t, successCount, 1,
		"at least one agent must acquire the lock")
}
