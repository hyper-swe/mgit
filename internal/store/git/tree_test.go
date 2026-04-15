package git

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

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
