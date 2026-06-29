package service

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// MGIT-42: `branch --delete` must clear BOTH the go-git ref and the SQLite index
// row, a stale index orphan (ref gone, row present) must self-heal on create,
// `worktree add` for a deleted task must succeed on the FIRST try, and a failed
// index write must not leave a partial git ref behind.

func TestDeleteBranch_ClearsBothRefAndIndex(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	br, err := env.branch.CreateBranch(ctx, "MGIT-1.1")
	require.NoError(t, err)
	_, err = env.idx.GetBranch(ctx, br.Name)
	require.NoError(t, err, "precondition: index row present")

	require.NoError(t, env.branch.DeleteBranch(ctx, br.Name, true))

	_, err = env.idx.GetBranch(ctx, br.Name)
	assert.ErrorIs(t, err, model.ErrBranchNotFound, "delete must clear the index row")
	_, err = env.bs.GetBranch(ctx, br.Name)
	assert.ErrorIs(t, err, model.ErrBranchNotFound, "delete must clear the ref")
}

func TestDeleteBranch_RefAlreadyGone_StillClearsIndex(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	br, err := env.branch.CreateBranch(ctx, "MGIT-1.1")
	require.NoError(t, err)
	// Stuck state: ref removed directly, index row orphaned.
	require.NoError(t, env.bs.DeleteBranch(ctx, br.Name, true))
	_, err = env.idx.GetBranch(ctx, br.Name)
	require.NoError(t, err, "precondition: orphaned index row present")

	// A re-delete must clear the orphan even though the ref is already gone.
	require.NoError(t, env.branch.DeleteBranch(ctx, br.Name, true))
	_, err = env.idx.GetBranch(ctx, br.Name)
	assert.ErrorIs(t, err, model.ErrBranchNotFound)
}

func TestCreateBranch_StaleIndexOrphan_SelfHeals(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	br, err := env.branch.CreateBranch(ctx, "MGIT-1.1")
	require.NoError(t, err)
	// Orphan: ref gone, index row remains.
	require.NoError(t, env.bs.DeleteBranch(ctx, br.Name, true))

	// Re-creating must self-heal the stale row and succeed, leaving both stores consistent.
	_, err = env.branch.CreateBranch(ctx, "MGIT-1.1")
	require.NoError(t, err, "create must self-heal a stale index orphan")
	_, err = env.bs.GetBranch(ctx, br.Name)
	require.NoError(t, err)
	_, err = env.idx.GetBranch(ctx, br.Name)
	require.NoError(t, err)
}

func TestWorktreeAdd_AfterBranchDelete_SucceedsFirstTry(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()
	writeProjectFile(t, env, "a.go", "v1\n")
	svc := newWorktreeSvcWithSync(env, "h1")

	wt, err := svc.Add(ctx, model.WorktreeAddOptions{Path: filepath.Join(t.TempDir(), "wt1"), TaskID: "MTIX-30.8"})
	require.NoError(t, err)
	require.NoError(t, svc.Remove(ctx, wt.Path, false))
	require.NoError(t, env.branch.DeleteBranch(ctx, wt.Branch, true))

	// The reporter's failing step — must succeed on the FIRST try (was:
	// "create branch in index: branch already exists").
	_, err = svc.Add(ctx, model.WorktreeAddOptions{Path: filepath.Join(t.TempDir(), "wt2"), TaskID: "MTIX-30.8"})
	require.NoError(t, err)
}

func TestCreateBranch_IndexWriteFails_RollsBackRef(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()
	// Force the index write to fail by closing the DB; create must not leave a
	// partial git ref behind (atomicity across the two stores).
	require.NoError(t, env.idx.Close())

	_, err := env.branch.CreateBranch(ctx, "MGIT-7.7")
	require.Error(t, err)
	_, gerr := env.bs.GetBranch(ctx, "task/MGIT-7.7")
	assert.ErrorIs(t, gerr, model.ErrBranchNotFound, "ref must be rolled back when the index write fails")
}
