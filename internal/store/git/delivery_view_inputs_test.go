package git

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/require"
)

func TestFastForwardDeliveredNeedsReadableHostView(t *testing.T) {
	for _, kind := range []string{"bound reference missing", "tracked file replaced by directory"} {
		t.Run(kind, func(t *testing.T) {
			repo, _, base := faultRepoWithCommit(t)
			const branch = "task/MGIT-1"
			ctx := t.Context()
			tip := createCommitWithFile(t, repo, "a.go", "updated", "MGIT-9.2")
			require.NoError(t, NewWorktreeStore(repo).Checkout(ctx, branch))
			path := filepath.Join(repo.Root(), "a.go")
			marker := filepath.Join(path, "marker")
			merge := NewMergeStore(repo)
			if kind == "bound reference missing" {
				original := repo.branchOverride
				repo.branchOverride = "absent-view"
				err := merge.FastForwardDelivered(ctx, branch, tip)
				repo.branchOverride = original
				require.ErrorIs(t, err, plumbing.ErrReferenceNotFound)
			} else {
				require.NoError(t, os.Rename(path, path+".saved"))
				require.NoError(t, os.Mkdir(path, 0700))
				require.NoError(t, os.WriteFile(marker, []byte("host content"), 0600))
				err := merge.FastForwardDelivered(ctx, branch, tip)
				var pathErr *fs.PathError
				require.ErrorAs(t, err, &pathErr)
				require.Equal(t, path, pathErr.Path)
				data, err := fs.ReadFile(os.DirFS(repo.Root()), "a.go/marker")
				require.NoError(t, err)
				require.Equal(t, "host content", string(data))
				require.NoError(t, os.Rename(path, path+".directory"))
				require.NoError(t, os.Rename(path+".saved", path))
			}
			head, err := repo.Head()
			require.NoError(t, err)
			require.Equal(t, base, head)
			data, err := fs.ReadFile(os.DirFS(repo.Root()), "a.go")
			require.NoError(t, err)
			require.Equal(t, "package a\n", string(data))
			require.NoError(t, merge.FastForwardDelivered(ctx, branch, tip))
			require.NoError(t, merge.VerifyDelivered(ctx, tip))
		})
	}
}
