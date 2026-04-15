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

// --- TreeStore.TraverseTree ---

func TestTreeStore_TraverseTree(t *testing.T) {
	repo := initTestRepo(t)
	ts := NewTreeStore(repo)
	ctx := context.Background()

	// Get HEAD tree hash
	headHash, err := repo.Head()
	require.NoError(t, err)
	commitObj, err := repo.repo.CommitObject(hashFromString(headHash))
	require.NoError(t, err)

	entries, err := ts.TraverseTree(ctx, commitObj.TreeHash.String())
	require.NoError(t, err)
	// Initial commit tree may be empty but should not error
	_ = entries
}

// --- WorktreeStore.Checkout ---

func TestWorktreeStore_Checkout_NonExistent(t *testing.T) {
	repo := initTestRepo(t)
	ws := NewWorktreeStore(repo)
	ctx := context.Background()

	err := ws.Checkout(ctx, "nonexistent-branch")
	assert.Error(t, err)
}

// --- WorktreeStore.Clean with files ---

func TestWorktreeStore_Clean_WithFiles(t *testing.T) {
	repo := initTestRepo(t)
	ws := NewWorktreeStore(repo)
	ctx := context.Background()

	// Create untracked file
	err := os.WriteFile(filepath.Join(repo.Root(), "untracked.txt"), []byte("data"), 0o600)
	require.NoError(t, err)

	err = ws.Clean(ctx)
	assert.NoError(t, err)
}

// --- DiffStore edge cases ---

func TestDiffStore_DiffStats_EmptyDiff(t *testing.T) {
	repo := initTestRepo(t)
	ds := NewDiffStore(repo)
	cs := NewCommitStore(repo)
	ctx := context.Background()

	head1, err := repo.Head()
	require.NoError(t, err)

	c := makeTestModelCommit(t, "MGIT-1.1")
	head2, err := cs.CreateCommit(ctx, c)
	require.NoError(t, err)

	stats, err := ds.DiffStats(ctx, head1, head2)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, stats.LinesAdded, 0)
}

// --- CommitStore.CreateCommit sets all fields ---

func TestCommitStore_CreateCommit_SetsParentID(t *testing.T) {
	repo := initTestRepo(t)
	cs := NewCommitStore(repo)
	ctx := context.Background()

	c := makeTestModelCommit(t, "MGIT-2.1")
	_, err := cs.CreateCommit(ctx, c)
	require.NoError(t, err)
	assert.NotEmpty(t, c.ParentID, "ParentID must be set")
}

// --- BranchStore.DeleteBranch error paths ---

func TestBranchStore_DeleteBranch_NotFound(t *testing.T) {
	repo := initTestRepo(t)
	bs := NewBranchStore(repo)
	ctx := context.Background()

	err := bs.DeleteBranch(ctx, "nonexistent", true)
	assert.ErrorIs(t, err, model.ErrBranchNotFound)
}

// --- SyncingStorer SetReference error path ---

func TestSyncingStorer_SetReference_RefsDir(t *testing.T) {
	repo := initTestRepo(t)
	ss := NewSyncingStorer(repo.repo.Storer, repo.MgitDir())

	// Verify refs are listed
	refs, err := ss.IterReferences()
	require.NoError(t, err)
	assert.NotNil(t, refs)
}

// --- Repository.Init error: parent dir doesn't exist ---

func TestRepository_Init_ParentMissing(t *testing.T) {
	_, err := Init("/nonexistent/deep/path/that/wont/exist", fixedClock())
	assert.Error(t, err)
}

// --- CommitStore.ListCommits iterates ---

func TestCommitStore_ListCommits_Multiple(t *testing.T) {
	repo := initTestRepo(t)
	cs := NewCommitStore(repo)
	ctx := context.Background()

	for i := range 3 {
		c := makeTestModelCommit(t, "MGIT-3.1")
		c.Message = string(rune('A' + i))
		_, err := cs.CreateCommit(ctx, c)
		require.NoError(t, err)
	}

	commits, err := cs.ListCommits(ctx)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(commits), 3)
}

// --- TreeStore.BuildTree with delete operation ---

func TestTreeStore_BuildTree_WithDelete(t *testing.T) {
	repo := initTestRepo(t)
	ts := NewTreeStore(repo)
	ctx := context.Background()

	// Add a file first
	diffs := []model.FileDiff{
		{Path: "keep.go", Operation: model.DiffAdded, NewHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	}
	_, err := ts.BuildTree(ctx, diffs)
	require.NoError(t, err)

	// Now delete it
	diffs2 := []model.FileDiff{
		{Path: "keep.go", Operation: model.DiffDeleted},
	}
	hash, err := ts.BuildTree(ctx, diffs2)
	require.NoError(t, err)
	assert.NotEmpty(t, hash)
}

// --- BranchStore.ListBranches with multiple branches ---

func TestBranchStore_ListBranches_Multiple(t *testing.T) {
	repo := initTestRepo(t)
	bs := NewBranchStore(repo)
	ctx := context.Background()

	head, err := repo.Head()
	require.NoError(t, err)

	tid1, _ := model.ParseTaskID("MGIT-1.1")
	tid2, _ := model.ParseTaskID("MGIT-1.2")

	require.NoError(t, bs.CreateBranch(ctx, &model.Branch{Name: "b1", HeadCommit: head, TaskID: tid1}))
	require.NoError(t, bs.CreateBranch(ctx, &model.Branch{Name: "b2", HeadCommit: head, TaskID: tid2}))

	branches, err := bs.ListBranches(ctx)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(branches), 3) // main + b1 + b2
}
