package git

import (
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/require"
)

func TestMergedTreeRequiresAllThreeInputTrees(t *testing.T) {
	repo, faults, base := faultRepoWithCommit(t)
	head := createCommitWithFile(t, repo, "ours", "left", "MGIT-1")
	require.NoError(t, NewWorktreeStore(repo).Checkout(t.Context(), "task/MGIT-1"))
	source := createCommitWithFile(t, repo, "theirs", "right", "MGIT-1")
	merge := NewMergeStore(repo)
	for _, input := range []struct{ name, commit string }{
		{"base", base}, {"head", head}, {"source", source},
	} {
		t.Run(input.name, func(t *testing.T) {
			c, err := repo.repo.CommitObject(plumbing.NewHash(input.commit))
			require.NoError(t, err)
			faults.failHash = c.TreeHash
			treeHash, err := merge.mergedTree(head, source)
			faults.failHash = plumbing.ZeroHash
			require.ErrorContains(t, err, "read tree for "+input.commit)
			require.Equal(t, plumbing.ZeroHash, treeHash)
			treeHash, err = merge.mergedTree(head, source)
			require.NoError(t, err)
			tree, err := repo.repo.TreeObject(treeHash)
			require.NoError(t, err)
			for path, want := range map[string]string{"a.go": "package a\n", "ours": "left", "theirs": "right"} {
				file, err := tree.File(path)
				require.NoError(t, err)
				contents, err := file.Contents()
				require.NoError(t, err)
				require.Equal(t, want, contents, path)
			}
		})
	}
}
