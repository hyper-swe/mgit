package index

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// The worktrees table's UNIQUE constraints are what ENFORCE the FR-16
// exclusivity rules between concurrent `mgit work` processes — they are the
// single point at which a race is decided, now that the repo lock no longer
// wraps a whole provision (MGIT-120). A loser must therefore learn WHICH rule
// refused it, by name, not read a raw SQLite constraint string.
// Refs: FR-16, MGIT-120

func TestInsertWorktree_DuplicateTaskID_ReturnsTaskAlreadyBound(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	first := &model.WorktreeInfo{Path: "/tmp/wt-a", Branch: "task/T-1", TaskID: "T-1", AgentID: "a1"}
	require.NoError(t, store.InsertWorktree(ctx, first))

	second := &model.WorktreeInfo{Path: "/tmp/wt-b", Branch: "other", TaskID: "T-1", AgentID: "a2"}
	err := store.InsertWorktree(ctx, second)

	require.ErrorIs(t, err, model.ErrTaskAlreadyBound)
	assert.Contains(t, err.Error(), "T-1")
	assert.Contains(t, err.Error(), "/tmp/wt-a", "the refusal must name the worktree already holding the task")
}

func TestInsertWorktree_DuplicateBranch_ReturnsBranchInUse(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	first := &model.WorktreeInfo{Path: "/tmp/wt-a", Branch: "task/SHARED", TaskID: "T-1", AgentID: "a1"}
	require.NoError(t, store.InsertWorktree(ctx, first))

	second := &model.WorktreeInfo{Path: "/tmp/wt-b", Branch: "task/SHARED", TaskID: "T-2", AgentID: "a2"}
	err := store.InsertWorktree(ctx, second)

	require.ErrorIs(t, err, model.ErrBranchInUse)
	assert.Contains(t, err.Error(), "task/SHARED")
	assert.Contains(t, err.Error(), "/tmp/wt-a", "the refusal must name the worktree already on the branch")
}

func TestInsertWorktree_DuplicatePath_ReturnsWorktreeExists(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	first := &model.WorktreeInfo{Path: "/tmp/wt-a", Branch: "task/T-1", TaskID: "T-1", AgentID: "a1"}
	require.NoError(t, store.InsertWorktree(ctx, first))

	second := &model.WorktreeInfo{Path: "/tmp/wt-a", Branch: "task/T-2", TaskID: "T-2", AgentID: "a2"}
	err := store.InsertWorktree(ctx, second)

	require.ErrorIs(t, err, model.ErrWorktreeExists)
	assert.Contains(t, err.Error(), "/tmp/wt-a")
}

// TestInsertWorktree_ConcurrentSameTask_ExactlyOneWinner drives the constraint
// the way the fix relies on it: many goroutines claiming one task against one
// store. Exactly one insert may succeed and every other must be refused by
// name — this is what keeps the narrowed lock safe. Refs: FR-16, MGIT-120
func TestInsertWorktree_ConcurrentSameTask_ExactlyOneWinner(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	const claimants = 8
	errs := make([]error, claimants)
	done := make(chan int, claimants)
	for i := range claimants {
		go func(idx int) {
			errs[idx] = store.InsertWorktree(ctx, &model.WorktreeInfo{
				Path:   "/tmp/wt-" + string(rune('a'+idx)),
				Branch: "branch-" + string(rune('a'+idx)),
				TaskID: "CONTESTED", AgentID: "agent",
			})
			done <- idx
		}(i)
	}
	for range claimants {
		<-done
	}

	winners := 0
	for _, err := range errs {
		if err == nil {
			winners++
			continue
		}
		assert.ErrorIs(t, err, model.ErrTaskAlreadyBound, "a loser must be refused by name")
	}
	assert.Equal(t, 1, winners, "exactly one claimant may bind the task")

	all, err := store.ListWorktrees(ctx)
	require.NoError(t, err)
	assert.Len(t, all, 1, "the registry must hold exactly one worktree for the contested task")
}
