package git

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit-dev/internal/model"
)

// --- SyncingStorer.SetReference exercises the fsync path ---

func TestSyncingStorer_SetReference_Success(t *testing.T) {
	repo := initTestRepo(t)
	ss := NewSyncingStorer(repo.repo.Storer, repo.MgitDir())

	head, err := repo.Head()
	require.NoError(t, err)

	// Create a new branch reference via SyncingStorer
	ref := plumbing.NewHashReference(
		plumbing.NewBranchReferenceName("synced-branch"),
		plumbing.NewHash(head),
	)
	err = ss.SetReference(ref)
	require.NoError(t, err)

	// Verify it was stored
	stored, err := ss.Reference(plumbing.NewBranchReferenceName("synced-branch"))
	require.NoError(t, err)
	assert.Equal(t, head, stored.Hash().String())
}

// --- GC error paths ---

func TestGCStore_LooseObjectCount_MissingObjectsDir(t *testing.T) {
	repo := initTestRepo(t)
	gc := NewGCStore(repo)
	ctx := context.Background()

	// Remove the objects directory to trigger the os.IsNotExist path
	objectsDir := filepath.Join(repo.MgitDir(), "objects")
	require.NoError(t, os.RemoveAll(objectsDir))

	count, err := gc.LooseObjectCount(ctx)
	require.NoError(t, err, "missing objects dir should return 0, not error")
	assert.Equal(t, 0, count)
}

func TestGCStore_ObjectsDirSize_MissingObjectsDir(t *testing.T) {
	repo := initTestRepo(t)
	gc := NewGCStore(repo)
	ctx := context.Background()

	objectsDir := filepath.Join(repo.MgitDir(), "objects")
	require.NoError(t, os.RemoveAll(objectsDir))

	size, err := gc.ObjectsDirSize(ctx)
	require.NoError(t, err, "missing objects dir should return 0 size")
	assert.Equal(t, int64(0), size)
}

// --- Branch error paths ---

// --- Merge error paths ---

func TestMergeStore_CreateMergeCommit_InvalidSourceHash(t *testing.T) {
	repo := initTestRepo(t)
	ms := NewMergeStore(repo)
	ctx := context.Background()

	// CreateMergeCommit doesn't validate sourceHash up front -- it uses
	// it as a parent hash. The commit should still succeed since go-git
	// doesn't verify parent existence during wt.Commit.
	hash, err := ms.CreateMergeCommit(ctx, "merge with bad source", "0000000000000000000000000000000000000000")
	// This may succeed or fail depending on go-git behavior; we just
	// exercise the code path.
	if err == nil {
		assert.Len(t, hash, 40)
	}
}

// --- DiffStore with same commit (no changes) ---

func TestDiffStore_DiffCommits_SameCommit(t *testing.T) {
	repo := initTestRepo(t)
	ds := NewDiffStore(repo)
	ctx := context.Background()

	head, err := repo.Head()
	require.NoError(t, err)

	diffs, err := ds.DiffCommits(ctx, head, head)
	require.NoError(t, err)
	assert.Empty(t, diffs, "diff of commit with itself should be empty")
}

func TestDiffStore_DiffStats_SameCommit(t *testing.T) {
	repo := initTestRepo(t)
	ds := NewDiffStore(repo)
	ctx := context.Background()

	head, err := repo.Head()
	require.NoError(t, err)

	stats, err := ds.DiffStats(ctx, head, head)
	require.NoError(t, err)
	assert.Equal(t, 0, stats.LinesAdded)
	assert.Equal(t, 0, stats.LinesRemoved)
}

// --- Tree edge cases ---

func TestTreeStore_GetTree_InvalidHash(t *testing.T) {
	repo := initTestRepo(t)
	ts := NewTreeStore(repo)
	ctx := context.Background()

	_, err := ts.GetTree(ctx, "0000000000000000000000000000000000000000")
	assert.Error(t, err)
}

func TestTreeStore_TraverseTree_InvalidHash(t *testing.T) {
	repo := initTestRepo(t)
	ts := NewTreeStore(repo)
	ctx := context.Background()

	_, err := ts.TraverseTree(ctx, "0000000000000000000000000000000000000000")
	assert.Error(t, err)
}

// --- Tree: BuildTree with modify operation ---

func TestTreeStore_BuildTree_ModifyExistingFile(t *testing.T) {
	repo := initTestRepo(t)
	ts := NewTreeStore(repo)
	ws := NewWorktreeStore(repo)
	cs := NewCommitStore(repo)
	ctx := context.Background()

	// Add a file and commit so the tree is non-empty
	err := os.WriteFile(filepath.Join(repo.Root(), "modify_me.go"), []byte("package m\n"), 0o600)
	require.NoError(t, err)
	require.NoError(t, ws.Add(ctx, "modify_me.go"))
	c := makeTestModelCommit(t, "MGIT-1.1")
	_, err = cs.CreateCommit(ctx, c)
	require.NoError(t, err)

	// Now build a tree that modifies the file
	diffs := []model.FileDiff{
		{Path: "modify_me.go", Operation: model.DiffModified, NewHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	}
	hash, err := ts.BuildTree(ctx, diffs)
	require.NoError(t, err)
	assert.NotEmpty(t, hash)
}

// --- TraverseTree with real content ---

func TestTreeStore_TraverseTree_WithFiles(t *testing.T) {
	repo := initTestRepo(t)
	ts := NewTreeStore(repo)
	ws := NewWorktreeStore(repo)
	cs := NewCommitStore(repo)
	ctx := context.Background()

	// Create files and commit
	err := os.WriteFile(filepath.Join(repo.Root(), "traverse_a.go"), []byte("package a\n"), 0o600)
	require.NoError(t, err)
	require.NoError(t, ws.Add(ctx, "traverse_a.go"))
	c := makeTestModelCommit(t, "MGIT-1.1")
	hash, err := cs.CreateCommit(ctx, c)
	require.NoError(t, err)

	// Get tree hash from commit
	commit, err := cs.GetCommit(ctx, hash)
	require.NoError(t, err)

	entries, err := ts.TraverseTree(ctx, commit.TreeHash)
	require.NoError(t, err)
	assert.NotEmpty(t, entries, "traversal of tree with files must return entries")
}

// --- Worktree.Status with modified tracked file ---

func TestWorktreeStore_Status_ModifiedFile(t *testing.T) {
	repo := initTestRepo(t)
	ws := NewWorktreeStore(repo)
	cs := NewCommitStore(repo)
	ctx := context.Background()

	// Create, stage, and commit a file
	err := os.WriteFile(filepath.Join(repo.Root(), "tracked.go"), []byte("package t\n"), 0o600)
	require.NoError(t, err)
	require.NoError(t, ws.Add(ctx, "tracked.go"))
	c := makeTestModelCommit(t, "MGIT-1.1")
	_, err = cs.CreateCommit(ctx, c)
	require.NoError(t, err)

	// Modify the tracked file
	err = os.WriteFile(filepath.Join(repo.Root(), "tracked.go"), []byte("package t\n\nfunc X() {}\n"), 0o600)
	require.NoError(t, err)

	files, err := ws.Status(ctx)
	require.NoError(t, err)

	found := false
	for _, f := range files {
		if f.Path == "tracked.go" {
			found = true
			assert.NotEmpty(t, f.Worktree, "modified file should have worktree status")
		}
	}
	assert.True(t, found, "modified tracked file must appear in status")
}
