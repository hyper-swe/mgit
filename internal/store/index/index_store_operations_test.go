package index

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit-dev/internal/model"
)

func TestStore_WriteTx_RollbackOnError(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	err := store.WriteTx(ctx, func(_ *sql.Tx) error {
		return fmt.Errorf("intentional error")
	})
	assert.Error(t, err)
}

func TestStore_ReadTx_ConcurrentReads(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Seed data
	require.NoError(t, store.AddCommitToTask(ctx, "MGIT-1.1", "h1", "ch1", "a1", 0))

	// Concurrent reads
	done := make(chan bool, 5)
	for range 5 {
		go func() {
			records, err := store.GetTaskCommits(ctx, "MGIT-1.1")
			assert.NoError(t, err)
			assert.Len(t, records, 1)
			done <- true
		}()
	}
	for range 5 {
		<-done
	}
}

func TestBranches_LockBranch_ExpiredReacquisition(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Lock with very short duration (already expired since clock is fixed)
	require.NoError(t, store.LockBranch(ctx, "main", "agent-01", 0))

	// Different agent should succeed since lock is expired
	err := store.LockBranch(ctx, "main", "agent-02", 30*time.Second)
	assert.NoError(t, err, "expired lock should allow re-acquisition")
}

func TestBranches_LockBranch_SameAgent(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	require.NoError(t, store.LockBranch(ctx, "main", "agent-01", 30*time.Second))

	// Same agent should be able to re-lock
	err := store.LockBranch(ctx, "main", "agent-01", 30*time.Second)
	assert.NoError(t, err)
}

func TestWorktreeInsert_DuplicatePath(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	wt := &model.WorktreeInfo{Path: "/tmp/dup", Branch: "b1", TaskID: "MGIT-1.1"}
	require.NoError(t, store.InsertWorktree(ctx, wt))

	wt2 := &model.WorktreeInfo{Path: "/tmp/dup", Branch: "b2", TaskID: "MGIT-1.2"}
	err := store.InsertWorktree(ctx, wt2)
	assert.Error(t, err, "duplicate path must be rejected")
}

func TestSchema_Reopen(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Store data
	require.NoError(t, store.AddCommitToTask(ctx, "MGIT-1.1", "abc", "ch1", "a1", 0))

	// Close and reopen
	dbPath := store.Path()
	_ = store.Close()

	store2, err := New(dbPath, fixedClock())
	require.NoError(t, err)
	defer func() { _ = store2.Close() }()

	// Verify data survived
	records, err := store2.GetTaskCommits(ctx, "MGIT-1.1")
	require.NoError(t, err)
	assert.Len(t, records, 1)
}

func TestTaskCommits_ContentHash_Stored(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	require.NoError(t, store.AddCommitToTask(ctx, "MGIT-2.1", "hash1", "sha256content", "agent-01", 0))

	records, err := store.GetTaskCommits(ctx, "MGIT-2.1")
	require.NoError(t, err)
	require.Len(t, records, 1)
	assert.Equal(t, "sha256content", records[0].ContentHash)
	assert.Equal(t, "agent-01", records[0].AgentID)
}
