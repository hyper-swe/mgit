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

// These tests pin changeToFileDiff's mapping of go-git tree-change actions to
// FileDiff path + operation. The MGIT-33 dogfood bug: the switch used magic
// ints 0/1/2, but go-git's merkletrie.Action is `_ = iota; Insert; Delete;
// Modify` (Insert=1, Delete=2, Modify=3) — so a real Insert(1) hit the Delete
// case and read the empty From side, yielding an empty-path "deleted" entry and
// a blank `mgit diff`/`squash --to-git` patch. The pre-existing tests only
// asserted len(diffs) != 0, so they passed despite the wrong path/op.
// Refs: MGIT-33

// TestDiffStore_DiffCommits_AddMapsToAddedWithPath proves an added file yields a
// real path + DiffAdded + NewHash (not an empty-path "deleted"). Refs: MGIT-33
func TestDiffStore_DiffCommits_AddMapsToAddedWithPath(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()
	ws := NewWorktreeStore(repo)
	cs := NewCommitStore(repo)
	ds := NewDiffStore(repo)

	before, err := repo.Head()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(repo.Root(), "added.go"), []byte("package a\n"), 0o600))
	require.NoError(t, ws.Add(ctx, "added.go"))
	after, err := cs.CreateCommit(ctx, makeTestModelCommit(t, "MGIT-33"))
	require.NoError(t, err)

	diffs, err := ds.DiffCommits(ctx, before, after)
	require.NoError(t, err)
	require.Len(t, diffs, 1)
	assert.Equal(t, "added.go", diffs[0].Path, "added file path must be set, not empty")
	assert.Equal(t, model.DiffAdded, diffs[0].Operation, "an insert maps to DiffAdded, not DiffDeleted")
	assert.NotEmpty(t, diffs[0].NewHash, "an added file carries its new blob hash")
}

// TestDiffStore_DiffCommits_ModifyMapsToModifiedWithPath proves a modified file
// yields a real path + DiffModified + both hashes. Refs: MGIT-33
func TestDiffStore_DiffCommits_ModifyMapsToModifiedWithPath(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()
	ws := NewWorktreeStore(repo)
	cs := NewCommitStore(repo)
	ds := NewDiffStore(repo)

	abs := filepath.Join(repo.Root(), "m.go")
	require.NoError(t, os.WriteFile(abs, []byte("package m\n"), 0o600))
	require.NoError(t, ws.Add(ctx, "m.go"))
	first, err := cs.CreateCommit(ctx, makeTestModelCommit(t, "MGIT-33"))
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(abs, []byte("package m\n\nvar X = 1\n"), 0o600))
	require.NoError(t, ws.Add(ctx, "m.go"))
	second, err := cs.CreateCommit(ctx, makeTestModelCommit(t, "MGIT-33"))
	require.NoError(t, err)

	diffs, err := ds.DiffCommits(ctx, first, second)
	require.NoError(t, err)
	require.Len(t, diffs, 1)
	assert.Equal(t, "m.go", diffs[0].Path)
	assert.Equal(t, model.DiffModified, diffs[0].Operation, "a modify maps to DiffModified")
	assert.NotEmpty(t, diffs[0].OldHash)
	assert.NotEmpty(t, diffs[0].NewHash)
}

// TestDiffStore_DiffCommits_DeleteMapsToDeletedWithPath proves a deleted file
// yields a real path + DiffDeleted + OldHash. Refs: MGIT-33
func TestDiffStore_DiffCommits_DeleteMapsToDeletedWithPath(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()
	ws := NewWorktreeStore(repo)
	cs := NewCommitStore(repo)
	ds := NewDiffStore(repo)

	abs := filepath.Join(repo.Root(), "d.go")
	require.NoError(t, os.WriteFile(abs, []byte("package d\n"), 0o600))
	require.NoError(t, ws.Add(ctx, "d.go"))
	first, err := cs.CreateCommit(ctx, makeTestModelCommit(t, "MGIT-33"))
	require.NoError(t, err)

	require.NoError(t, os.Remove(abs))
	require.NoError(t, ws.Add(ctx, "d.go")) // stage the deletion
	second, err := cs.CreateCommit(ctx, makeTestModelCommit(t, "MGIT-33"))
	require.NoError(t, err)

	diffs, err := ds.DiffCommits(ctx, first, second)
	require.NoError(t, err)
	require.Len(t, diffs, 1)
	assert.Equal(t, "d.go", diffs[0].Path)
	assert.Equal(t, model.DiffDeleted, diffs[0].Operation, "a delete maps to DiffDeleted")
	assert.NotEmpty(t, diffs[0].OldHash)
}
