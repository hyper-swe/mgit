package git

import (
	"context"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// buildTreeEntryMode builds a tree from the given diffs and returns the
// filemode of the named entry, for asserting BuildTree's mode fidelity.
func buildTreeEntryMode(t *testing.T, ts *TreeStore, diffs []model.FileDiff, name string) filemode.FileMode {
	t.Helper()
	hash, err := ts.BuildTree(context.Background(), diffs)
	require.NoError(t, err)
	tree, err := ts.repo.repo.TreeObject(plumbing.NewHash(hash))
	require.NoError(t, err)
	entry, err := tree.FindEntry(name)
	require.NoError(t, err)
	return entry.Mode
}

func TestTreeStore_BuildTree_PreservesExecutableMode(t *testing.T) {
	repo := initTestRepo(t)
	ts := NewTreeStore(repo)

	diffs := []model.FileDiff{
		{
			Path:      "build.sh",
			Operation: model.DiffAdded,
			NewHash:   "1111111111111111111111111111111111111111",
			Mode:      model.FileModeExecutable,
		},
	}

	mode := buildTreeEntryMode(t, ts, diffs, "build.sh")
	assert.Equal(t, filemode.Executable, mode,
		"an executable diff entry must yield mode 100755")
}

func TestTreeStore_BuildTree_PreservesSymlinkMode(t *testing.T) {
	repo := initTestRepo(t)
	ts := NewTreeStore(repo)

	diffs := []model.FileDiff{
		{
			Path:      "link",
			Operation: model.DiffAdded,
			NewHash:   "2222222222222222222222222222222222222222",
			Mode:      model.FileModeSymlink,
		},
	}

	mode := buildTreeEntryMode(t, ts, diffs, "link")
	assert.Equal(t, filemode.Symlink, mode,
		"a symlink diff entry must yield mode 120000")
}

func TestTreeStore_BuildTree_UnsetModeIsRegular(t *testing.T) {
	repo := initTestRepo(t)
	ts := NewTreeStore(repo)

	// No Mode set — must behave exactly as before (regular file, 100644).
	diffs := []model.FileDiff{
		{
			Path:      "readme.txt",
			Operation: model.DiffAdded,
			NewHash:   "3333333333333333333333333333333333333333",
		},
	}

	mode := buildTreeEntryMode(t, ts, diffs, "readme.txt")
	assert.Equal(t, filemode.Regular, mode,
		"an unset-mode diff entry must default to regular 100644")
}

func TestTreeStore_BuildTree(t *testing.T) {
	repo := initTestRepo(t)
	ts := NewTreeStore(repo)
	ctx := context.Background()

	diffs := []model.FileDiff{
		{Path: "main.go", Operation: model.DiffAdded, NewHash: "abcdef1234567890abcdef1234567890abcdef12"},
	}

	hash, err := ts.BuildTree(ctx, diffs)
	require.NoError(t, err, "BuildTree must succeed")
	assert.Len(t, hash, 40, "tree hash must be 40 chars")
}

func TestTreeStore_BuildTree_FromDiffs(t *testing.T) {
	repo := initTestRepo(t)
	ts := NewTreeStore(repo)
	ctx := context.Background()

	diffs := []model.FileDiff{
		{Path: "foo.go", Operation: model.DiffAdded, NewHash: "1111111111111111111111111111111111111111"},
		{Path: "bar.go", Operation: model.DiffAdded, NewHash: "2222222222222222222222222222222222222222"},
	}

	hash, err := ts.BuildTree(ctx, diffs)
	require.NoError(t, err)

	// Verify we can get the tree back
	entries, err := ts.GetTree(ctx, hash)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(entries), 2, "tree must have at least 2 entries")
}

func TestTreeStore_GetTree(t *testing.T) {
	repo := initTestRepo(t)
	ts := NewTreeStore(repo)
	ctx := context.Background()

	// Get HEAD commit's tree
	headHash, err := repo.Head()
	require.NoError(t, err)

	commitObj, err := repo.repo.CommitObject(hashFromString(headHash))
	require.NoError(t, err)

	entries, err := ts.GetTree(ctx, commitObj.TreeHash.String())
	require.NoError(t, err)
	assert.NotNil(t, entries)
}

func TestTreeStore_TreeHashes_Deterministic(t *testing.T) {
	repo := initTestRepo(t)
	ts := NewTreeStore(repo)
	ctx := context.Background()

	diffs := []model.FileDiff{
		{Path: "x.go", Operation: model.DiffAdded, NewHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	}

	hash1, err := ts.BuildTree(ctx, diffs)
	require.NoError(t, err)
	hash2, err := ts.BuildTree(ctx, diffs)
	require.NoError(t, err)

	assert.Equal(t, hash1, hash2, "same diffs must produce same tree hash")
}
