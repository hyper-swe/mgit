package git

import (
	"io/fs"
	"os"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/hyper-swe/mgit/internal/model"
	"github.com/stretchr/testify/require"
)

func TestRestorePlanNeedsReadableHistory(t *testing.T) {
	repo, faults, target := faultRepoWithCommit(t)
	tip := createCommitWithFile(t, repo, "a.go", "new version", "MGIT-9.2")
	stageOneFile(t, repo, "a.go", "local version")
	c, err := repo.repo.CommitObject(plumbing.NewHash(target))
	require.NoError(t, err)
	for _, tc := range []struct {
		name string
		hash plumbing.Hash
		want string
	}{
		{"target commit", c.Hash, "load target " + target},
		{"target tree", c.TreeHash, "target tree"},
		{"current commit", plumbing.NewHash(tip), "get HEAD commit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			faults.failHash = tc.hash
			plan, local, err := repo.ComputeRestoreDiffs(target)
			faults.failHash = plumbing.ZeroHash
			require.ErrorContains(t, err, tc.want)
			require.Empty(t, plan)
			require.Empty(t, local)
		})
	}
	plan, local, err := repo.ComputeRestoreDiffs(target)
	require.NoError(t, err)
	require.Len(t, plan, 1)
	require.Equal(t, "a.go", plan[0].Path)
	require.Equal(t, model.DiffModified, plan[0].Operation)
	require.Equal(t, plumbing.ComputeHash(plumbing.BlobObject, []byte("package a\n")).String(), plan[0].NewHash)
	require.Equal(t, []string{"a.go"}, local)

	staged, err := repo.StagedSnapshot()
	require.NoError(t, err)
	require.Equal(t, []string{"a.go"}, staged)
	head, err := repo.Head()
	require.NoError(t, err)
	require.Equal(t, tip, head)
	data, err := fs.ReadFile(os.DirFS(repo.Root()), "a.go")
	require.NoError(t, err)
	require.Equal(t, "local version", string(data))
}
