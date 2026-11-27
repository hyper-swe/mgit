package git

import (
	"io/fs"
	"os"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/require"
)

func TestDeliveryDoesNotPublishWithoutRequiredInputs(t *testing.T) {
	for _, input := range []string{"target tree", "target blob", "staging", "generated file list"} {
		t.Run(input, func(t *testing.T) {
			repo, faults, base := faultRepoWithCommit(t)
			ctx := t.Context()
			const branch = "task/MGIT-1"
			tip := createCommitWithFile(t, repo, "a.go", "new contents", "MGIT-9.2")
			c, err := repo.repo.CommitObject(plumbing.NewHash(tip))
			require.NoError(t, err)
			require.NoError(t, NewWorktreeStore(repo).Checkout(ctx, branch))

			var restore func()
			var want string
			switch input {
			case "target tree", "target blob":
				faults.failHash = c.TreeHash
				if input == "target blob" {
					faults.failHash = plumbing.ComputeHash(plumbing.BlobObject, []byte("new contents"))
				}
				restore = func() { faults.failHash = plumbing.ZeroHash }
				want = "object read failure"
			case "staging":
				path := repo.stagingPath()
				require.NoError(t, os.WriteFile(path, []byte("{"), 0o600))
				restore = func() { require.NoError(t, os.Remove(path)) }
				want = "decode staging"
			case "generated file list":
				path := generatedManifestPath(repo.Root())
				require.NoError(t, os.Mkdir(path, 0o700))
				restore = func() { require.NoError(t, os.Remove(path)) }
				want = "generated manifest"
			}

			merge := NewMergeStore(repo)
			err = merge.FastForwardDelivered(ctx, branch, tip)
			restore()
			require.ErrorContains(t, err, want)
			head, err := repo.Head()
			require.NoError(t, err)
			require.Equal(t, base, head)
			read := func() string {
				t.Helper()
				data, err := fs.ReadFile(os.DirFS(repo.Root()), "a.go")
				require.NoError(t, err)
				return string(data)
			}
			require.Equal(t, "package a\n", read())
			require.NoError(t, merge.FastForwardDelivered(ctx, branch, tip))
			head, err = repo.Head()
			require.NoError(t, err)
			require.Equal(t, tip, head)
			require.Equal(t, "new contents", read())
		})
	}
}
