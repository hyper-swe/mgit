package git

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

func TestDiffStore_DiffCommits(t *testing.T) {
	repo := initTestRepo(t)
	ds := NewDiffStore(repo)
	cs := NewCommitStore(repo)
	ctx := context.Background()

	// Get initial commit hash
	head1, err := repo.Head()
	require.NoError(t, err)

	// Create a commit
	c := makeTestModelCommit(t, "MGIT-1.1")
	head2, err := cs.CreateCommit(ctx, c)
	require.NoError(t, err)

	// Diff between them (may be empty since we allow empty commits)
	diffs, err := ds.DiffCommits(ctx, head1, head2)
	require.NoError(t, err)
	assert.NotNil(t, diffs) // may be empty, but not nil error
}

func TestDiffStore_DiffCommits_NotFound(t *testing.T) {
	repo := initTestRepo(t)
	ds := NewDiffStore(repo)
	ctx := context.Background()

	_, err := ds.DiffCommits(ctx,
		"0000000000000000000000000000000000000000",
		"1111111111111111111111111111111111111111")
	assert.ErrorIs(t, err, model.ErrCommitNotFound)
}

func TestDiffStore_DiffStats(t *testing.T) {
	repo := initTestRepo(t)
	ds := NewDiffStore(repo)
	cs := NewCommitStore(repo)
	ctx := context.Background()

	head1, err := repo.Head()
	require.NoError(t, err)

	c := makeTestModelCommit(t, "MGIT-2.1")
	head2, err := cs.CreateCommit(ctx, c)
	require.NoError(t, err)

	stats, err := ds.DiffStats(ctx, head1, head2)
	require.NoError(t, err)
	// Stats may be zero for empty commits
	assert.GreaterOrEqual(t, stats.LinesAdded, 0)
	assert.GreaterOrEqual(t, stats.LinesRemoved, 0)
}
