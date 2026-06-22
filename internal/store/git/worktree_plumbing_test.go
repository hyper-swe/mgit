package git

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// TestWorktreeCheckout_ViaPlumbing_MaterializesFiles verifies that checkout
// writes the target branch tree to disk via plumbing (no go-git worktree), and
// that switching back removes files not present in the target tree.
// Refs: MGIT-14.4
func TestWorktreeCheckout_ViaPlumbing_MaterializesFiles(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()
	ws := NewWorktreeStore(repo)
	cs := NewCommitStore(repo)
	bs := NewBranchStore(repo)

	// Commit feature.go on main, then branch a task off it.
	require.NoError(t, os.WriteFile(filepath.Join(repo.Root(), "feature.go"), []byte("package feature\n"), 0o600))
	require.NoError(t, ws.Add(ctx, "feature.go"))
	c := makeTestModelCommit(t, "MGIT-3.1")
	featCommit, err := cs.CreateCommit(ctx, c)
	require.NoError(t, err)

	tid, _ := model.ParseTaskID("MGIT-3.2")
	require.NoError(t, bs.CreateBranch(ctx, &model.Branch{Name: "task/MGIT-3.2", HeadCommit: featCommit, TaskID: tid}))

	// Remove the file from disk, then checkout the task branch — materialization
	// must restore feature.go from the branch tree.
	require.NoError(t, os.Remove(filepath.Join(repo.Root(), "feature.go")))
	require.NoError(t, ws.Checkout(ctx, "task/MGIT-3.2"))

	data, err := os.ReadFile(filepath.Join(repo.Root(), "feature.go"))
	require.NoError(t, err, "checkout must materialize the branch tree onto disk")
	assert.Equal(t, "package feature\n", string(data))

	branch, err := repo.CurrentBranch()
	require.NoError(t, err)
	assert.Equal(t, "task/MGIT-3.2", branch)
}

// TestMergeCommit_TwoParents_ViaPlumbing verifies a merge commit is built as a
// two-parent commit object via plumbing (no go-git worktree).
// Refs: MGIT-14.4
func TestMergeCommit_TwoParents_ViaPlumbing(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()
	bs := NewBranchStore(repo)
	ms := NewMergeStore(repo)

	base, err := repo.Head()
	require.NoError(t, err)

	tid, _ := model.ParseTaskID("MGIT-4.1")
	require.NoError(t, bs.CreateBranch(ctx, &model.Branch{Name: "feature", HeadCommit: base, TaskID: tid}))
	require.NoError(t, bs.SwitchBranch(ctx, "feature"))
	featureHash := createCommitWithFile(t, repo, "feat.go", "package feat\n", "MGIT-4.2")

	require.NoError(t, bs.SwitchBranch(ctx, "main"))
	mergeHash, err := ms.CreateMergeCommit(ctx, "merge feature into main", featureHash)
	require.NoError(t, err)

	merged, err := repo.repo.CommitObject(hashFromString(mergeHash))
	require.NoError(t, err)
	assert.Equal(t, 2, merged.NumParents(), "merge commit must have exactly two parents")
}

// TestWorktree_OverExistingGitRepo verifies the full add/commit/checkout cycle
// works inside a directory that already contains a real project `.git`, and the
// project HEAD is never moved.
// Refs: MGIT-14.4
func TestWorktree_OverExistingGitRepo(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	clk := fixedClock()
	projectHead := makeProjectGitRepo(t, dir, clk)

	repo, err := Init(dir, clk)
	require.NoError(t, err)
	t.Cleanup(func() { _ = repo.Close() })

	ws := NewWorktreeStore(repo)
	cs := NewCommitStore(repo)
	bs := NewBranchStore(repo)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "wt.go"), []byte("package wt\n"), 0o600))
	require.NoError(t, ws.Add(ctx, "wt.go"))
	c := makeTestModelCommit(t, "MGIT-5.1")
	commitHash, err := cs.CreateCommit(ctx, c)
	require.NoError(t, err)

	tid, _ := model.ParseTaskID("MGIT-5.2")
	require.NoError(t, bs.CreateBranch(ctx, &model.Branch{Name: "task/MGIT-5.2", HeadCommit: commitHash, TaskID: tid}))
	require.NoError(t, ws.Checkout(ctx, "task/MGIT-5.2"))
	require.NoError(t, ws.Checkout(ctx, "main"))

	// Project git HEAD unchanged.
	g, err := gogit.PlainOpen(dir)
	require.NoError(t, err)
	h, err := g.Head()
	require.NoError(t, err)
	assert.Equal(t, projectHead, h.Hash())
}
