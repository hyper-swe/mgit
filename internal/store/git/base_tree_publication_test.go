package git

import (
	"errors"
	"io/fs"
	"os"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/require"
)

func TestRecordExistingTreePublishesOnlyAfterAllObjectsAreWritten(t *testing.T) {
	for _, kind := range []plumbing.ObjectType{plumbing.BlobObject, plumbing.TreeObject, plumbing.CommitObject} {
		t.Run(kind.String(), func(t *testing.T) {
			repo, _ := newFaultRepo(t)
			before, err := repo.Head()
			require.NoError(t, err)
			writeFile(t, repo, "notes", "original bytes")
			store := repo.repo.Storer
			cause := errors.New("base object unavailable")
			repo.repo.Storer = snapshotWriteFault{Storer: store, kind: kind, err: cause}
			err = repo.recordExistingTreeAsBase()
			repo.repo.Storer = store
			require.ErrorIs(t, err, cause)
			head, err := repo.Head()
			require.NoError(t, err)
			require.Equal(t, before, head)
			data, err := fs.ReadFile(os.DirFS(repo.Root()), "notes")
			require.NoError(t, err)
			require.Equal(t, "original bytes", string(data))

			require.NoError(t, repo.recordExistingTreeAsBase())
			next, err := repo.Head()
			require.NoError(t, err)
			cs := NewCommitStore(repo)
			parents, err := cs.Parents(t.Context(), next)
			require.NoError(t, err)
			require.Equal(t, []string{before}, parents)
			data, err = cs.GetFileFromCommit(t.Context(), next, "notes")
			require.NoError(t, err)
			require.Equal(t, "original bytes", string(data))
		})
	}
}
