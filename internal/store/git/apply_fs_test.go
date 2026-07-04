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

// TestDirtyPaths_Classification covers every dirty-state class the
// content-applying verbs guard on. Refs: MGIT-54
func TestDirtyPaths_Classification(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()
	writeAndCommit(t, repo, "MGIT-1", map[string]string{
		"clean.txt":   "c\n",
		"edited.txt":  "original\n",
		"deleted.txt": "d\n",
	})

	// staged: a new file explicitly staged.
	require.NoError(t, os.WriteFile(filepath.Join(repo.Root(), "staged.txt"), []byte("s\n"), 0o600))
	require.NoError(t, NewWorktreeStore(repo).Add(ctx, "staged.txt"))
	// edited: tracked file locally modified, uncommitted.
	require.NoError(t, os.WriteFile(filepath.Join(repo.Root(), "edited.txt"), []byte("changed\n"), 0o600))
	// deleted: tracked file removed locally, uncommitted.
	require.NoError(t, os.Remove(filepath.Join(repo.Root(), "deleted.txt")))
	// untracked: on disk, never committed, not staged.
	require.NoError(t, os.WriteFile(filepath.Join(repo.Root(), "untracked.txt"), []byte("u\n"), 0o600))

	dirty, err := repo.DirtyPaths([]string{
		"clean.txt", "staged.txt", "edited.txt", "deleted.txt", "untracked.txt", "absent.txt",
	})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"staged.txt", "edited.txt", "deleted.txt", "untracked.txt"}, dirty,
		"clean tracked files and absent paths are not dirty; every uncommitted local state is")
}

// TestMaterializeDiffs_WritesAndDeletes: applied diffs land on disk — adds and
// modifies written from blobs, deletes removed; unrelated files untouched.
// Refs: MGIT-54
func TestMaterializeDiffs_WritesAndDeletes(t *testing.T) {
	repo := initTestRepo(t)
	ds := NewDiffStore(repo)
	ctx := context.Background()

	v1 := writeAndCommit(t, repo, "MGIT-1", map[string]string{"a.txt": "v1\n", "gone.txt": "g\n"})
	v2 := writeAndCommit(t, repo, "MGIT-1", map[string]string{"a.txt": "v2\n"})
	require.NoError(t, os.Remove(filepath.Join(repo.Root(), "gone.txt")))
	require.NoError(t, NewWorktreeStore(repo).Add(ctx, "."))
	c := makeTestModelCommit(t, "MGIT-1")
	c.FileDiffs = nil
	v3, err := NewCommitStore(repo).CreateCommit(ctx, c)
	require.NoError(t, err)
	_ = v2

	// v3 -> v1: restores a.txt to v1 and brings gone.txt back.
	diffs, err := ds.DiffCommits(ctx, v3, v1)
	require.NoError(t, err)
	require.NoError(t, repo.MaterializeDiffs(diffs))

	b, err := os.ReadFile(filepath.Join(repo.Root(), "a.txt")) //nolint:gosec // test path
	require.NoError(t, err)
	assert.Equal(t, "v1\n", string(b))
	b, err = os.ReadFile(filepath.Join(repo.Root(), "gone.txt")) //nolint:gosec // test path
	require.NoError(t, err)
	assert.Equal(t, "g\n", string(b))

	// And the reverse direction deletes gone.txt again.
	back, err := ds.DiffCommits(ctx, v1, v3)
	require.NoError(t, err)
	require.NoError(t, repo.MaterializeDiffs(back))
	_, statErr := os.Stat(filepath.Join(repo.Root(), "gone.txt"))
	assert.True(t, os.IsNotExist(statErr))
}

// TestMaterializeDiffs_RejectsEscapingPath: path validation runs before any
// write. Refs: MGIT-54, NFR-5
func TestMaterializeDiffs_RejectsEscapingPath(t *testing.T) {
	repo := initTestRepo(t)
	err := repo.MaterializeDiffs([]model.FileDiff{
		{Path: "../escape.txt", Operation: model.DiffAdded, NewHash: "abc"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid path")
}

// TestIsAncestorOfHead: tip and ancestors report true; a commit on a side
// branch (the squash-artifact shape) reports false. Refs: MGIT-54, MGIT-22
func TestIsAncestorOfHead(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()

	base := writeAndCommit(t, repo, "MGIT-1", map[string]string{"a.txt": "1\n"})
	tip := writeAndCommit(t, repo, "MGIT-1", map[string]string{"a.txt": "2\n"})

	// A commit on a side branch forked from base.
	bs := NewBranchStore(repo)
	require.NoError(t, bs.CreateBranch(ctx, branchModel("side", base)))
	require.NoError(t, bs.SwitchBranch(ctx, "side"))
	side := writeAndCommit(t, repo, "MGIT-2", map[string]string{"b.txt": "s\n"})
	require.NoError(t, bs.SwitchBranch(ctx, "main"))

	for name, tc := range map[string]struct {
		hash string
		want bool
	}{
		"tip_itself":                        {tip, true},
		"ancestor":                          {base, true},
		"side_branch_commit_not_on_lineage": {side, false},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := repo.IsAncestorOfHead(tc.hash)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}
