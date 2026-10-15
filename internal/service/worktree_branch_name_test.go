package service

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// `mgit work --branch feature/custom` for a branch that does not exist used to
// create task/<id> instead (the branch service always names it that way), fail
// to materialize the requested name, and leave the wrong branch behind, which a
// retry then pinned silently as its fork base. It is now refused up front, naming
// the problem, before anything is created. Refs: MGIT-127
func TestWorktreeService_Add_ANewBranchNameThatIsNotTheTaskBranchIsRefusedBeforeAnythingIsCreated(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()
	wtSvc := NewWorktreeService(env.idx, env.branch, env.wt, fixedClock())

	_, err := wtSvc.Add(ctx, model.WorktreeAddOptions{
		Path: filepath.Join(t.TempDir(), "wt"), TaskID: "MGIT-12.7", Branch: "feature/custom",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "feature/custom", "the refusal names the branch that was asked for")
	assert.Contains(t, err.Error(), "task/MGIT-12.7", "and the branch mgit would have created instead")

	_, getErr := env.branch.GetBranch(ctx, "task/MGIT-12.7")
	assert.Error(t, getErr, "a refused provision leaves no task branch behind")
	_, getErr = env.branch.GetBranch(ctx, "feature/custom")
	assert.Error(t, getErr)
	_, wtErr := env.idx.GetWorktreeByTask(ctx, "MGIT-12.7")
	assert.ErrorIs(t, wtErr, model.ErrWorktreeNotFound, "and no worktree is registered")
}

// The default name, and a requested name that is the task branch, still work.
func TestWorktreeService_Add_TheTaskBranchNameStillWorks(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()
	wtSvc := NewWorktreeService(env.idx, env.branch, env.wt, fixedClock())

	wt, err := wtSvc.Add(ctx, model.WorktreeAddOptions{Path: filepath.Join(t.TempDir(), "a"), TaskID: "MGIT-12.8"})
	require.NoError(t, err)
	assert.Equal(t, "task/MGIT-12.8", wt.Branch)

	wt, err = wtSvc.Add(ctx, model.WorktreeAddOptions{Path: filepath.Join(t.TempDir(), "b"), TaskID: "MGIT-12.9", Branch: "task/MGIT-12.9"})
	require.NoError(t, err)
	assert.Equal(t, "task/MGIT-12.9", wt.Branch)
}

// The refusal must name a command that works: mgit can only create a task's
// own branch, so "create X first" is advice nobody can follow. Refs: MGIT-127
func TestWorktreeService_Add_TheRefusalNamesTheCommandsThatWork(t *testing.T) {
	env := setupTestEnv(t)
	wtSvc := NewWorktreeService(env.idx, env.branch, env.wt, fixedClock())

	_, err := wtSvc.Add(context.Background(), model.WorktreeAddOptions{
		Path: filepath.Join(t.TempDir(), "wt"), TaskID: "MGIT-12.7", Branch: "feature/custom",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rerun without --branch")
	assert.Contains(t, err.Error(), "--branch task/MGIT-12.7")
	assert.NotContains(t, err.Error(), "create \"feature/custom\" first", "mgit cannot create that branch, so it must not be suggested")
}

// An EXISTING branch under any name is still accepted: the refusal is only for
// a name that does not exist and cannot be created. Refs: MGIT-127
func TestWorktreeService_Add_AnExistingNonTaskBranchIsStillAccepted(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()
	_, err := env.branch.GetBranch(ctx, "main")
	require.NoError(t, err, "the fixture's main branch exists and is not a task branch")
	wtSvc := NewWorktreeService(env.idx, env.branch, env.wt, fixedClock())

	wt, err := wtSvc.Add(ctx, model.WorktreeAddOptions{Path: filepath.Join(t.TempDir(), "wt"), TaskID: "MGIT-12.10", Branch: "main"})
	require.NoError(t, err)
	assert.Equal(t, "main", wt.Branch)
}
