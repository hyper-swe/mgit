package index

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit-dev/internal/model"
)

func TestBranches_CreateBranch(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	taskID, err := model.ParseTaskID("MGIT-1.2")
	require.NoError(t, err)

	branch := &model.Branch{
		Name:       "task/MGIT-1.2",
		TaskID:     taskID,
		HeadCommit: "abc123",
	}

	err = store.CreateBranch(ctx, branch)
	assert.NoError(t, err)
}

func TestBranches_CreateBranch_Duplicate(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	taskID, err := model.ParseTaskID("MGIT-1.2")
	require.NoError(t, err)

	branch := &model.Branch{
		Name:       "task/MGIT-1.2",
		TaskID:     taskID,
		HeadCommit: "abc123",
	}

	require.NoError(t, store.CreateBranch(ctx, branch))
	err = store.CreateBranch(ctx, branch)
	assert.ErrorIs(t, err, model.ErrBranchAlreadyExists)
}

func TestBranches_GetBranch(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	taskID, err := model.ParseTaskID("MGIT-2.1")
	require.NoError(t, err)

	err = store.CreateBranch(ctx, &model.Branch{
		Name:       "task/MGIT-2.1",
		TaskID:     taskID,
		HeadCommit: "def456",
	})
	require.NoError(t, err)

	b, err := store.GetBranch(ctx, "task/MGIT-2.1")
	require.NoError(t, err)
	assert.Equal(t, "task/MGIT-2.1", b.Name)
	assert.Equal(t, "def456", b.HeadCommit)
	assert.Equal(t, taskID, b.TaskID)
}

func TestBranches_GetBranch_NotFound(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	_, err := store.GetBranch(ctx, "nonexistent")
	assert.ErrorIs(t, err, model.ErrBranchNotFound)
}

func TestBranches_UpdateBranchHead(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	taskID, err := model.ParseTaskID("MGIT-3.1")
	require.NoError(t, err)

	err = store.CreateBranch(ctx, &model.Branch{
		Name:       "task/MGIT-3.1",
		TaskID:     taskID,
		HeadCommit: "hash1",
	})
	require.NoError(t, err)

	err = store.UpdateBranchHead(ctx, "task/MGIT-3.1", "hash2")
	assert.NoError(t, err)

	b, err := store.GetBranch(ctx, "task/MGIT-3.1")
	require.NoError(t, err)
	assert.Equal(t, "hash2", b.HeadCommit)
}

func TestBranches_UpdateBranchHead_NotFound(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	err := store.UpdateBranchHead(ctx, "nonexistent", "hash")
	assert.ErrorIs(t, err, model.ErrBranchNotFound)
}

func TestBranches_LockBranch(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	err := store.LockBranch(ctx, "task/MGIT-1.2", "agent-01", 30*time.Second)
	assert.NoError(t, err)
}

func TestBranches_LockBranch_AlreadyLocked(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	require.NoError(t, store.LockBranch(ctx, "task/MGIT-1.2", "agent-01", 30*time.Second))

	// Different agent should fail
	err := store.LockBranch(ctx, "task/MGIT-1.2", "agent-02", 30*time.Second)
	assert.ErrorIs(t, err, model.ErrBranchLocked)
}

func TestBranches_UnlockBranch(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	require.NoError(t, store.LockBranch(ctx, "task/MGIT-1.2", "agent-01", 30*time.Second))

	err := store.UnlockBranch(ctx, "task/MGIT-1.2")
	assert.NoError(t, err)

	// After unlock, another agent should be able to lock
	err = store.LockBranch(ctx, "task/MGIT-1.2", "agent-02", 30*time.Second)
	assert.NoError(t, err)
}

func TestBranches_ListBranches(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	tid1, _ := model.ParseTaskID("MGIT-1.1")
	tid2, _ := model.ParseTaskID("MGIT-1.2")

	require.NoError(t, store.CreateBranch(ctx, &model.Branch{Name: "main", TaskID: tid1, HeadCommit: "h1"}))
	require.NoError(t, store.CreateBranch(ctx, &model.Branch{Name: "task/MGIT-1.2", TaskID: tid2, HeadCommit: "h2"}))

	branches, err := store.ListBranches(ctx)
	require.NoError(t, err)
	assert.Len(t, branches, 2)
}
