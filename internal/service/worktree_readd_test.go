package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// The paths of existingForkBase the CLI scenario does not reach: a re-added
// task branch with no commits yet, a service wired without a commit store,
// and a --base that names nothing. The scenario with commits is
// e2e/worktree_readd_e2e_test.go. Refs: MGIT-275

// loseWorktree removes a worktree's registration and directory, as a lost
// directory followed by `mgit worktree prune` does.
func loseWorktree(t *testing.T, env *testEnv, wt *model.WorktreeInfo) {
	t.Helper()
	require.NoError(t, env.idx.DeleteWorktree(context.Background(), wt.Path))
	require.NoError(t, os.RemoveAll(wt.Path))
}

func TestWorktreeAdd_ReAddedBranchWithoutCommits_PinsItsTip(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()
	svc := newWorktreeSvcWithSync(env, "head-1")
	opts := model.WorktreeAddOptions{Path: filepath.Join(t.TempDir(), "wt"), TaskID: "MGIT-1.1"}
	first, err := svc.Add(ctx, opts)
	require.NoError(t, err)
	loseWorktree(t, env, first)

	again, err := svc.Add(ctx, opts)
	require.NoError(t, err)
	assert.Equal(t, first.ForkBase, again.ForkBase,
		"a task branch with no commits still sits at its fork-base")
}

func TestWorktreeAdd_ReAddWithoutCommitStore_KeepsTheTipAndRefusesBase(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()
	opts := model.WorktreeAddOptions{Path: filepath.Join(t.TempDir(), "wt"), TaskID: "MGIT-1.1"}
	first, err := newWorktreeSvcWithSync(env, "head-1").Add(ctx, opts)
	require.NoError(t, err)
	loseWorktree(t, env, first)
	bare := NewWorktreeService(env.idx, env.branch, env.wt, fixedClock())

	withBase := opts
	withBase.Base = first.ForkBase
	_, err = bare.Add(ctx, withBase)
	require.Error(t, err, "--base cannot be checked without a commit store")

	again, err := bare.Add(ctx, opts)
	require.NoError(t, err)
	br, err := env.branch.GetBranch(ctx, again.Branch)
	require.NoError(t, err)
	assert.Equal(t, br.HeadCommit, again.ForkBase)
}

func TestWorktreeAdd_ReAddWithUnknownBase_Refused(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()
	svc := newWorktreeSvcWithSync(env, "head-1")
	opts := model.WorktreeAddOptions{Path: filepath.Join(t.TempDir(), "wt"), TaskID: "MGIT-1.1"}
	first, err := svc.Add(ctx, opts)
	require.NoError(t, err)
	loseWorktree(t, env, first)

	opts.Base = "no-such-ref"
	_, err = svc.Add(ctx, opts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no-such-ref")
}

// With task commits on record, the fork-base is the first commit's parent,
// and --base is held to it.
func TestWorktreeAdd_ReAddAfterTaskCommits_PinsTheFirstCommitsParent(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()
	svc := newWorktreeSvcWithSync(env, "head-1")
	opts := model.WorktreeAddOptions{Path: filepath.Join(t.TempDir(), "wt"), TaskID: "MGIT-1.1"}
	first, err := svc.Add(ctx, opts)
	require.NoError(t, err)
	loseWorktree(t, env, first)
	before, err := env.repo.Head()
	require.NoError(t, err)
	commitFile(t, env, "MGIT-1.1", "one.go", "package one\n")
	after, err := env.repo.Head()
	require.NoError(t, err)

	wrong := opts
	wrong.Base = after
	_, err = svc.Add(ctx, wrong)
	require.ErrorIs(t, err, model.ErrInvalidCommit, "a --base other than the fork-base is refused")
	assert.Contains(t, err.Error(), before[:12], "the refusal names the fork-base")

	right := opts
	right.Base = before
	again, err := svc.Add(ctx, right)
	require.NoError(t, err)
	assert.Equal(t, before, again.ForkBase, "the fork-base is the first task commit's parent, not the tip")
}
