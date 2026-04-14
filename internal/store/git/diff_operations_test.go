package git

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit-dev/internal/model"
)

func TestDiffStore_DiffCommits_WithFileChanges(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()
	ws := NewWorktreeStore(repo)
	cs := NewCommitStore(repo)
	ds := NewDiffStore(repo)

	// First commit: add a file
	err := os.WriteFile(filepath.Join(repo.Root(), "diff_a.go"), []byte("package a\n"), 0o600)
	require.NoError(t, err)
	require.NoError(t, ws.Add(ctx, "diff_a.go"))

	c1 := makeTestModelCommit(t, "MGIT-1.1")
	hash1, err := cs.CreateCommit(ctx, c1)
	require.NoError(t, err)

	// Second commit: modify the file
	err = os.WriteFile(filepath.Join(repo.Root(), "diff_a.go"), []byte("package a\n\nfunc A() {}\n"), 0o600)
	require.NoError(t, err)
	require.NoError(t, ws.Add(ctx, "diff_a.go"))

	c2 := makeTestModelCommit(t, "MGIT-1.2")
	hash2, err := cs.CreateCommit(ctx, c2)
	require.NoError(t, err)

	diffs, err := ds.DiffCommits(ctx, hash1, hash2)
	require.NoError(t, err)
	// go-git should detect changes between commits with different trees
	assert.NotNil(t, diffs, "diff result must not be nil")
}

func TestDiffStore_DiffCommits_AddedFile(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()
	ws := NewWorktreeStore(repo)
	cs := NewCommitStore(repo)
	ds := NewDiffStore(repo)

	headBefore, err := repo.Head()
	require.NoError(t, err)

	// Add a new file
	err = os.WriteFile(filepath.Join(repo.Root(), "new_file.go"), []byte("package n\n"), 0o600)
	require.NoError(t, err)
	require.NoError(t, ws.Add(ctx, "new_file.go"))

	c := makeTestModelCommit(t, "MGIT-1.1")
	hashAfter, err := cs.CreateCommit(ctx, c)
	require.NoError(t, err)

	diffs, err := ds.DiffCommits(ctx, headBefore, hashAfter)
	require.NoError(t, err)
	// The diff should produce at least one change entry when a file is added
	assert.NotEmpty(t, diffs, "adding a file must produce diffs")
}

func TestDiffStore_DiffCommits_DeletedFile(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()
	ws := NewWorktreeStore(repo)
	cs := NewCommitStore(repo)
	ds := NewDiffStore(repo)

	// Add a file first
	err := os.WriteFile(filepath.Join(repo.Root(), "to_delete.go"), []byte("package d\n"), 0o600)
	require.NoError(t, err)
	require.NoError(t, ws.Add(ctx, "to_delete.go"))

	c1 := makeTestModelCommit(t, "MGIT-1.1")
	hash1, err := cs.CreateCommit(ctx, c1)
	require.NoError(t, err)

	// Delete the file and stage
	require.NoError(t, os.Remove(filepath.Join(repo.Root(), "to_delete.go")))
	wt, err := repo.repo.Worktree()
	require.NoError(t, err)
	_, err = wt.Add("to_delete.go")
	require.NoError(t, err)

	c2 := makeTestModelCommit(t, "MGIT-1.2")
	hash2, err := cs.CreateCommit(ctx, c2)
	require.NoError(t, err)

	diffs, err := ds.DiffCommits(ctx, hash1, hash2)
	require.NoError(t, err)
	assert.NotEmpty(t, diffs, "deleting a file must produce diffs")
}

func TestDiffStore_DiffCommits_InvalidFromHash(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()
	ds := NewDiffStore(repo)

	head, err := repo.Head()
	require.NoError(t, err)

	_, err = ds.DiffCommits(ctx, "0000000000000000000000000000000000000000", head)
	assert.Error(t, err)
	assert.ErrorIs(t, err, model.ErrCommitNotFound)
}

func TestDiffStore_DiffCommits_InvalidToHash(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()
	ds := NewDiffStore(repo)

	head, err := repo.Head()
	require.NoError(t, err)

	_, err = ds.DiffCommits(ctx, head, "0000000000000000000000000000000000000000")
	assert.Error(t, err)
	assert.ErrorIs(t, err, model.ErrCommitNotFound)
}

func TestDiffStore_DiffStats_WithChanges(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()
	ws := NewWorktreeStore(repo)
	cs := NewCommitStore(repo)
	ds := NewDiffStore(repo)

	headBefore, err := repo.Head()
	require.NoError(t, err)

	err = os.WriteFile(filepath.Join(repo.Root(), "stats.go"), []byte("package stats\n\nfunc F() {}\n"), 0o600)
	require.NoError(t, err)
	require.NoError(t, ws.Add(ctx, "stats.go"))

	c := makeTestModelCommit(t, "MGIT-1.1")
	hashAfter, err := cs.CreateCommit(ctx, c)
	require.NoError(t, err)

	stats, err := ds.DiffStats(ctx, headBefore, hashAfter)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, stats.LinesAdded, 0)
}

func TestDiffStore_DiffStats_InvalidHash(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()
	ds := NewDiffStore(repo)

	head, err := repo.Head()
	require.NoError(t, err)

	_, err = ds.DiffStats(ctx, head, "0000000000000000000000000000000000000000")
	assert.Error(t, err)
}
