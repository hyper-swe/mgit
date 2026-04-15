package git

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// createCommitWithFile creates a commit that adds or modifies a file in the
// repo worktree. Returns the SHA-1 hash of the new commit.
func createCommitWithFile(t *testing.T, repo *Repository, filename, content, taskID string) string {
	t.Helper()
	ctx := context.Background()
	ws := NewWorktreeStore(repo)
	cs := NewCommitStore(repo)

	err := os.WriteFile(filepath.Join(repo.Root(), filename), []byte(content), 0o600)
	require.NoError(t, err)

	require.NoError(t, ws.Add(ctx, filename))

	c := makeTestModelCommit(t, taskID)
	c.Message = "[MGIT:" + taskID + "] add " + filename
	hash, err := cs.CreateCommit(ctx, c)
	require.NoError(t, err)
	return hash
}

func TestMergeStore_NewMergeStore(t *testing.T) {
	repo := initTestRepo(t)
	ms := NewMergeStore(repo)
	assert.NotNil(t, ms, "NewMergeStore must return non-nil")
}

func TestMergeStore_IsAncestor_TrueForParent(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()

	parent, err := repo.Head()
	require.NoError(t, err)

	child := createCommitWithFile(t, repo, "a.go", "package a\n", "MGIT-1.1")

	ms := NewMergeStore(repo)
	ok, err := ms.IsAncestor(ctx, parent, child)
	require.NoError(t, err)
	assert.True(t, ok, "parent must be ancestor of child")
}

func TestMergeStore_IsAncestor_FalseForReverse(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()

	parent, err := repo.Head()
	require.NoError(t, err)

	child := createCommitWithFile(t, repo, "a.go", "package a\n", "MGIT-1.1")

	ms := NewMergeStore(repo)
	ok, err := ms.IsAncestor(ctx, child, parent)
	require.NoError(t, err)
	assert.False(t, ok, "child must not be ancestor of parent")
}

func TestMergeStore_IsAncestor_InvalidAncestor(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()
	ms := NewMergeStore(repo)

	child, err := repo.Head()
	require.NoError(t, err)

	_, err = ms.IsAncestor(ctx, "0000000000000000000000000000000000000000", child)
	assert.Error(t, err)
	assert.ErrorIs(t, err, model.ErrCommitNotFound)
}

func TestMergeStore_IsAncestor_InvalidDescendant(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()
	ms := NewMergeStore(repo)

	ancestor, err := repo.Head()
	require.NoError(t, err)

	_, err = ms.IsAncestor(ctx, ancestor, "0000000000000000000000000000000000000000")
	assert.Error(t, err)
	assert.ErrorIs(t, err, model.ErrCommitNotFound)
}

func TestMergeStore_MergeBase_CommonAncestor(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()
	bs := NewBranchStore(repo)
	ms := NewMergeStore(repo)

	// Initial commit is the common ancestor
	baseHash, err := repo.Head()
	require.NoError(t, err)

	// Create branch "feature" from current HEAD
	tid, _ := model.ParseTaskID("MGIT-1.1")
	require.NoError(t, bs.CreateBranch(ctx, &model.Branch{Name: "feature", HeadCommit: baseHash, TaskID: tid}))

	// Commit on main
	mainHash := createCommitWithFile(t, repo, "main.go", "package main\n", "MGIT-1.2")

	// Switch to feature and commit
	require.NoError(t, bs.SwitchBranch(ctx, "feature"))
	featureHash := createCommitWithFile(t, repo, "feat.go", "package feat\n", "MGIT-1.3")

	base, err := ms.MergeBase(ctx, mainHash, featureHash)
	require.NoError(t, err)
	assert.Equal(t, baseHash, base, "merge base should be the initial divergence point")
}

func TestMergeStore_MergeBase_InvalidLeft(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()
	ms := NewMergeStore(repo)

	head, err := repo.Head()
	require.NoError(t, err)

	_, err = ms.MergeBase(ctx, "0000000000000000000000000000000000000000", head)
	assert.Error(t, err)
	assert.ErrorIs(t, err, model.ErrCommitNotFound)
}

func TestMergeStore_MergeBase_InvalidRight(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()
	ms := NewMergeStore(repo)

	head, err := repo.Head()
	require.NoError(t, err)

	_, err = ms.MergeBase(ctx, head, "0000000000000000000000000000000000000000")
	assert.Error(t, err)
	assert.ErrorIs(t, err, model.ErrCommitNotFound)
}

func TestMergeStore_FastForward_Success(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()
	bs := NewBranchStore(repo)
	ms := NewMergeStore(repo)

	baseHash, err := repo.Head()
	require.NoError(t, err)

	// Create a branch at the initial commit
	tid, _ := model.ParseTaskID("MGIT-1.1")
	require.NoError(t, bs.CreateBranch(ctx, &model.Branch{Name: "target", HeadCommit: baseHash, TaskID: tid}))

	// Advance main
	advancedHash := createCommitWithFile(t, repo, "ff.go", "package ff\n", "MGIT-1.2")

	// Fast-forward "target" to advanced commit
	err = ms.FastForward(ctx, "target", advancedHash)
	require.NoError(t, err)

	// Verify branch now points to new hash
	branch, err := bs.GetBranch(ctx, "target")
	require.NoError(t, err)
	assert.Equal(t, advancedHash, branch.HeadCommit)
}

func TestMergeStore_FastForward_BranchNotFound(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()
	ms := NewMergeStore(repo)

	head, err := repo.Head()
	require.NoError(t, err)

	err = ms.FastForward(ctx, "nonexistent", head)
	assert.Error(t, err)
	assert.ErrorIs(t, err, model.ErrBranchNotFound)
}

func TestMergeStore_CreateMergeCommit_Success(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()
	bs := NewBranchStore(repo)
	ms := NewMergeStore(repo)
	cs := NewCommitStore(repo)

	baseHash, err := repo.Head()
	require.NoError(t, err)

	// Create feature branch and commit there
	tid, _ := model.ParseTaskID("MGIT-1.1")
	require.NoError(t, bs.CreateBranch(ctx, &model.Branch{Name: "feature", HeadCommit: baseHash, TaskID: tid}))
	require.NoError(t, bs.SwitchBranch(ctx, "feature"))
	featureHash := createCommitWithFile(t, repo, "feat.go", "package feat\n", "MGIT-1.2")

	// Switch back to main
	require.NoError(t, bs.SwitchBranch(ctx, "main"))

	// Create merge commit
	mergeHash, err := ms.CreateMergeCommit(ctx, "merge feature into main", featureHash)
	require.NoError(t, err)
	assert.NotEmpty(t, mergeHash)
	assert.Len(t, mergeHash, 40, "SHA-1 must be 40 hex chars")

	// Verify the merge commit has two parents
	merged, err := cs.GetCommit(ctx, mergeHash)
	require.NoError(t, err)
	assert.Contains(t, merged.Message, "merge feature")
}

func TestMergeStore_ConflictingPaths_NoConflict(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()
	bs := NewBranchStore(repo)
	ms := NewMergeStore(repo)

	baseHash, err := repo.Head()
	require.NoError(t, err)

	// Branch feature from base
	tid, _ := model.ParseTaskID("MGIT-1.1")
	require.NoError(t, bs.CreateBranch(ctx, &model.Branch{Name: "feature", HeadCommit: baseHash, TaskID: tid}))

	// Commit different files on main
	mainHash := createCommitWithFile(t, repo, "main_only.go", "package main\n", "MGIT-1.2")

	// Switch to feature and commit a different file
	require.NoError(t, bs.SwitchBranch(ctx, "feature"))
	featureHash := createCommitWithFile(t, repo, "feat_only.go", "package feat\n", "MGIT-1.3")

	conflicts, err := ms.ConflictingPaths(ctx, baseHash, mainHash, featureHash)
	require.NoError(t, err)
	assert.Empty(t, conflicts, "no conflicts when different files are modified")
}

func TestMergeStore_ConflictingPaths_WithConflict(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()
	bs := NewBranchStore(repo)
	ms := NewMergeStore(repo)

	// Create a base file first
	baseCommit := createCommitWithFile(t, repo, "shared.go", "package shared\n", "MGIT-1.1")

	// Branch feature from this commit
	tid, _ := model.ParseTaskID("MGIT-1.2")
	require.NoError(t, bs.CreateBranch(ctx, &model.Branch{Name: "feature", HeadCommit: baseCommit, TaskID: tid}))

	// Modify shared.go on main
	mainHash := createCommitWithFile(t, repo, "shared.go", "package shared // main version\n", "MGIT-1.3")

	// Switch to feature, modify shared.go differently
	require.NoError(t, bs.SwitchBranch(ctx, "feature"))
	featureHash := createCommitWithFile(t, repo, "shared.go", "package shared // feature version\n", "MGIT-1.4")

	conflicts, err := ms.ConflictingPaths(ctx, baseCommit, mainHash, featureHash)
	require.NoError(t, err)
	assert.Contains(t, conflicts, "shared.go", "shared.go must appear as conflicting")
}

func TestMergeStore_ConflictingPaths_InvalidBase(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()
	ms := NewMergeStore(repo)

	head, err := repo.Head()
	require.NoError(t, err)

	_, err = ms.ConflictingPaths(ctx, "0000000000000000000000000000000000000000", head, head)
	assert.Error(t, err)
	assert.ErrorIs(t, err, model.ErrCommitNotFound)
}

func TestMergeStore_ConflictingPaths_InvalidLeft(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()
	ms := NewMergeStore(repo)

	head, err := repo.Head()
	require.NoError(t, err)

	_, err = ms.ConflictingPaths(ctx, head, "0000000000000000000000000000000000000000", head)
	assert.Error(t, err)
	assert.ErrorIs(t, err, model.ErrCommitNotFound)
}

func TestMergeStore_ConflictingPaths_InvalidRight(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()
	ms := NewMergeStore(repo)

	head, err := repo.Head()
	require.NoError(t, err)

	_, err = ms.ConflictingPaths(ctx, head, head, "0000000000000000000000000000000000000000")
	assert.Error(t, err)
	assert.ErrorIs(t, err, model.ErrCommitNotFound)
}

func TestMergeStore_CommitObject_NotFound(t *testing.T) {
	repo := initTestRepo(t)
	ms := NewMergeStore(repo)

	_, err := ms.commitObject("0000000000000000000000000000000000000000")
	assert.Error(t, err)
	assert.ErrorIs(t, err, model.ErrCommitNotFound)
}
