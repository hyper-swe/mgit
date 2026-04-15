package index

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// Re-export for test readability
var ErrAppendOnlyViolation = model.ErrAppendOnlyViolation

func TestTaskCommits_AddCommitToTask(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	err := store.AddCommitToTask(ctx, "MGIT-1.2.3", "abc123", "sha256hash", "agent-01", 0)
	assert.NoError(t, err, "AddCommitToTask must succeed")
}

func TestTaskCommits_AddCommitToTask_Duplicate_Rejected(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	err := store.AddCommitToTask(ctx, "MGIT-1.2.3", "abc123", "sha256", "agent-01", 0)
	require.NoError(t, err)

	// Same task_id + commit_hash should fail (UNIQUE constraint)
	err = store.AddCommitToTask(ctx, "MGIT-1.2.3", "abc123", "sha256", "agent-01", 1)
	assert.Error(t, err, "duplicate (task_id, commit_hash) must be rejected")
}

func TestTaskCommits_GetTaskCommits(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Add 3 commits to a task
	require.NoError(t, store.AddCommitToTask(ctx, "MGIT-2.1", "hash1", "ch1", "agent-01", 0))
	require.NoError(t, store.AddCommitToTask(ctx, "MGIT-2.1", "hash2", "ch2", "agent-01", 1))
	require.NoError(t, store.AddCommitToTask(ctx, "MGIT-2.1", "hash3", "ch3", "agent-01", 2))

	records, err := store.GetTaskCommits(ctx, "MGIT-2.1")
	require.NoError(t, err)
	assert.Len(t, records, 3)

	// Verify ordering by position
	assert.Equal(t, 0, records[0].Position)
	assert.Equal(t, 1, records[1].Position)
	assert.Equal(t, 2, records[2].Position)
	assert.Equal(t, "hash1", records[0].CommitHash)
	assert.Equal(t, "hash3", records[2].CommitHash)
}

func TestTaskCommits_GetTaskCommits_Empty(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	records, err := store.GetTaskCommits(ctx, "MGIT-99.99")
	require.NoError(t, err)
	assert.Empty(t, records)
}

func TestTaskCommits_GetCommitTask(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	require.NoError(t, store.AddCommitToTask(ctx, "MGIT-3.1", "commithash1", "ch1", "agent-01", 0))

	taskID, err := store.GetCommitTask(ctx, "commithash1")
	require.NoError(t, err)
	assert.Equal(t, "MGIT-3.1", taskID)
}

func TestTaskCommits_GetCommitTask_NotFound(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	_, err := store.GetCommitTask(ctx, "nonexistent")
	assert.ErrorIs(t, err, model.ErrTaskNotFound)
}

func TestTaskCommits_DeleteRejected(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	err := store.DeleteFromTask(ctx, "MGIT-1", "hash")
	assert.ErrorIs(t, err, model.ErrAppendOnlyViolation,
		"DeleteFromTask must always return ErrAppendOnlyViolation")
}
