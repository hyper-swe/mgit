package index

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

func TestWorktreeInsert_Valid(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	wt := &model.WorktreeInfo{
		Path: "/tmp/wt1", Branch: "task/MGIT-1.2",
		TaskID: "MGIT-1.2", AgentID: "agent-01",
		CreatedAt: time.Date(2026, 4, 7, 12, 0, 0, 0, time.UTC),
	}
	err := store.InsertWorktree(ctx, wt)
	assert.NoError(t, err)
}

func TestWorktreeInsert_DuplicateTaskID(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	wt1 := &model.WorktreeInfo{Path: "/tmp/wt1", Branch: "b1", TaskID: "MGIT-1.2", AgentID: "a1"}
	require.NoError(t, store.InsertWorktree(ctx, wt1))

	wt2 := &model.WorktreeInfo{Path: "/tmp/wt2", Branch: "b2", TaskID: "MGIT-1.2", AgentID: "a2"}
	err := store.InsertWorktree(ctx, wt2)
	assert.Error(t, err, "duplicate task_id must be rejected")
}

func TestWorktreeGet_Exists(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	wt := &model.WorktreeInfo{Path: "/tmp/wt1", Branch: "task/MGIT-2.1", TaskID: "MGIT-2.1", AgentID: "a1"}
	require.NoError(t, store.InsertWorktree(ctx, wt))

	got, err := store.GetWorktree(ctx, "/tmp/wt1")
	require.NoError(t, err)
	assert.Equal(t, "MGIT-2.1", got.TaskID)
	assert.Equal(t, "task/MGIT-2.1", got.Branch)
}

func TestWorktreeGet_NotFound(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	_, err := store.GetWorktree(ctx, "/nonexistent")
	assert.ErrorIs(t, err, model.ErrWorktreeNotFound)
}

func TestWorktreeList_Empty(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	wts, err := store.ListWorktrees(ctx)
	require.NoError(t, err)
	assert.Empty(t, wts)
}

func TestWorktreeList_Multiple(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	require.NoError(t, store.InsertWorktree(ctx, &model.WorktreeInfo{Path: "/tmp/a", Branch: "b1", TaskID: "MGIT-1.1"}))
	require.NoError(t, store.InsertWorktree(ctx, &model.WorktreeInfo{Path: "/tmp/b", Branch: "b2", TaskID: "MGIT-1.2"}))

	wts, err := store.ListWorktrees(ctx)
	require.NoError(t, err)
	assert.Len(t, wts, 2)
}

func TestWorktreeDelete_Exists(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	require.NoError(t, store.InsertWorktree(ctx, &model.WorktreeInfo{Path: "/tmp/del", Branch: "b", TaskID: "MGIT-3.1"}))

	err := store.DeleteWorktree(ctx, "/tmp/del")
	assert.NoError(t, err)

	_, err = store.GetWorktree(ctx, "/tmp/del")
	assert.ErrorIs(t, err, model.ErrWorktreeNotFound)
}

func TestWorktreeDelete_NotFound(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	err := store.DeleteWorktree(ctx, "/nonexistent")
	assert.ErrorIs(t, err, model.ErrWorktreeNotFound)
}
