package git

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/require"
)

func TestChangedPathsRequiresReadableTreesAndRetainsDeletions(t *testing.T) {
	repo, faults := newFaultRepo(t)
	base := writeAndCommit(t, repo, "MGIT-1", map[string]string{"old": "before", "dir/note": "first", "unchanged": "keep"})
	require.NoError(t, os.Remove(filepath.Join(repo.Root(), "old")))
	target := writeAndCommit(t, repo, "MGIT-1", map[string]string{"dir/note": "updated"})
	left, err := repo.repo.CommitObject(plumbing.NewHash(base))
	require.NoError(t, err)
	right, err := repo.repo.CommitObject(plumbing.NewHash(target))
	require.NoError(t, err)
	tree, err := right.Tree()
	require.NoError(t, err)
	nested, err := tree.Tree("dir")
	require.NoError(t, err)
	for _, tc := range []struct {
		name    string
		hash    plumbing.Hash
		context string
	}{
		{"base", left.TreeHash, "base tree"},
		{"target", right.TreeHash, "target tree"},
		{"subtree during diff", nested.Hash, "tree diff"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			faults.failHash = tc.hash
			paths, err := changedPaths(left, right)
			faults.failHash = plumbing.ZeroHash
			require.ErrorContains(t, err, tc.context)
			require.Nil(t, paths, "unreadable input must not look like a partial change set")
			paths, err = changedPaths(left, right)
			require.NoError(t, err)
			require.Equal(t, map[string]string{"old": "", "dir/note": plumbing.ComputeHash(plumbing.BlobObject, []byte("updated")).String()}, paths)
		})
	}
}
