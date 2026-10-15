package index

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// A worktree lookup that fails for a reason other than "no such row" must say
// so. Callers branch on ErrWorktreeNotFound to mean "this task was never given
// a worktree"; a closed or failing index read as that answer gives a confident
// wrong result from a dead database. Refs: MGIT-116
func TestGetWorktree_ABrokenStoreIsNotReportedAsNotFound(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	require.NoError(t, store.InsertWorktree(ctx, &model.WorktreeInfo{Path: "/tmp/wt1", Branch: "b", TaskID: "MGIT-1", AgentID: "a"}))
	require.NoError(t, store.Close())

	_, err := store.GetWorktree(ctx, "/tmp/wt1")
	require.Error(t, err)
	assert.False(t, errors.Is(err, model.ErrWorktreeNotFound), "a closed index must not read as 'not registered': %v", err)

	_, err = store.GetWorktreeByTask(ctx, "MGIT-1")
	require.Error(t, err)
	assert.False(t, errors.Is(err, model.ErrWorktreeNotFound), "a closed index must not read as 'no worktree for the task': %v", err)
}

func TestGetWorktree_ACancelledContextIsNotReportedAsNotFound(t *testing.T) {
	store := newTestStore(t)
	require.NoError(t, store.InsertWorktree(context.Background(), &model.WorktreeInfo{Path: "/tmp/wt1", Branch: "b", TaskID: "MGIT-1", AgentID: "a"}))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := store.GetWorktreeByTask(ctx, "MGIT-1")
	require.Error(t, err)
	assert.False(t, errors.Is(err, model.ErrWorktreeNotFound), "a cancelled read must not read as 'no worktree': %v", err)
	assert.True(t, errors.Is(err, context.Canceled), "the cause must stay recoverable with errors.Is: %v", err)
}

// The genuine not-found answer is unchanged. Refs: MGIT-116
func TestGetWorktree_AMissingRowIsStillNotFound(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	_, err := store.GetWorktree(ctx, "/tmp/never")
	assert.ErrorIs(t, err, model.ErrWorktreeNotFound)
	_, err = store.GetWorktreeByTask(ctx, "MGIT-404")
	assert.ErrorIs(t, err, model.ErrWorktreeNotFound)
}
