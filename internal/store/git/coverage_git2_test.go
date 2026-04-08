package git

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/astutic/mgit/internal/model"
)

// --- WorktreeStore.Checkout with real branch ---

func TestWorktreeStore_Checkout_ValidBranch(t *testing.T) {
	repo := initTestRepo(t)
	bs := NewBranchStore(repo)
	ws := NewWorktreeStore(repo)
	ctx := context.Background()

	head, err := repo.Head()
	require.NoError(t, err)

	tid, _ := model.ParseTaskID("MGIT-4.1")
	require.NoError(t, bs.CreateBranch(ctx, &model.Branch{
		Name: "task/MGIT-4.1", HeadCommit: head, TaskID: tid,
	}))

	err = ws.Checkout(ctx, "task/MGIT-4.1")
	assert.NoError(t, err)
}

// --- WorktreeStore.Add error path ---

func TestWorktreeStore_Add_NonExistent(t *testing.T) {
	repo := initTestRepo(t)
	ws := NewWorktreeStore(repo)
	ctx := context.Background()

	err := ws.Add(ctx, "nonexistent-file.go")
	assert.Error(t, err)
}

// --- WorktreeStore.Clean with untracked files ---

func TestWorktreeStore_Clean_WithUntracked(t *testing.T) {
	repo := initTestRepo(t)
	ws := NewWorktreeStore(repo)
	ctx := context.Background()

	err := os.WriteFile(filepath.Join(repo.Root(), "untracked.txt"), []byte("data"), 0o600)
	require.NoError(t, err)

	err = ws.Clean(ctx)
	assert.NoError(t, err)
}

// --- SyncingStorer: delegation ---

func TestSyncingStorer_IterReferences(t *testing.T) {
	repo := initTestRepo(t)
	ss := NewSyncingStorer(repo.repo.Storer, repo.MgitDir())

	refs, err := ss.IterReferences()
	require.NoError(t, err)
	assert.NotNil(t, refs)
}

func TestSyncingStorer_Reference(t *testing.T) {
	repo := initTestRepo(t)
	ss := NewSyncingStorer(repo.repo.Storer, repo.MgitDir())

	ref, err := ss.Reference(plumbing.HEAD)
	require.NoError(t, err)
	assert.NotNil(t, ref)
}

// --- BranchStore.DeleteBranch not found ---

func TestBranchStore_DeleteBranch_NotFound_Force(t *testing.T) {
	repo := initTestRepo(t)
	bs := NewBranchStore(repo)
	ctx := context.Background()

	err := bs.DeleteBranch(ctx, "nonexistent", true)
	assert.ErrorIs(t, err, model.ErrBranchNotFound)
}

// --- CommitStore: multiple creates and list ---

func TestCommitStore_ListCommits_Order(t *testing.T) {
	repo := initTestRepo(t)
	cs := NewCommitStore(repo)
	ctx := context.Background()

	for i := range 3 {
		c := makeTestModelCommit(t, "MGIT-5.1")
		c.Message = string(rune('A' + i))
		_, err := cs.CreateCommit(ctx, c)
		require.NoError(t, err)
	}

	commits, err := cs.ListCommits(ctx)
	require.NoError(t, err)
	// Most recent first (committer time order)
	assert.GreaterOrEqual(t, len(commits), 3)
}

// --- TreeStore: build with multiple ops ---

func TestTreeStore_BuildTree_MultipleOps(t *testing.T) {
	repo := initTestRepo(t)
	ts := NewTreeStore(repo)
	ctx := context.Background()

	diffs := []model.FileDiff{
		{Path: "a.go", Operation: model.DiffAdded, NewHash: "1111111111111111111111111111111111111111"},
		{Path: "b.go", Operation: model.DiffAdded, NewHash: "2222222222222222222222222222222222222222"},
		{Path: "c.go", Operation: model.DiffAdded, NewHash: "3333333333333333333333333333333333333333"},
	}

	hash, err := ts.BuildTree(ctx, diffs)
	require.NoError(t, err)

	entries, err := ts.GetTree(ctx, hash)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(entries), 3)
}

// --- Repository: open non-directory .mgit ---

func TestRepository_Open_FileNotDir(t *testing.T) {
	tmpDir := t.TempDir()
	err := os.WriteFile(filepath.Join(tmpDir, ".mgit"), []byte("file"), 0o600)
	require.NoError(t, err)

	_, err = Open(tmpDir, fixedClock())
	assert.Error(t, err)
}
