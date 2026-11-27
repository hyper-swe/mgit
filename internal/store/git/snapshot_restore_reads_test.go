package git

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/storage"
	"github.com/stretchr/testify/require"
)

// Let lookup read the named object, then fail when restore needs it again.
type snapshotRestoreReadFault struct {
	storage.Storer
	hash plumbing.Hash
	seen bool
	err  error
}

func (s *snapshotRestoreReadFault) EncodedObject(typ plumbing.ObjectType, hash plumbing.Hash) (plumbing.EncodedObject, error) {
	if hash == s.hash {
		if s.seen {
			return nil, s.err
		}
		s.seen = true
	}
	return s.Storer.EncodedObject(typ, hash)
}

func TestSnapshotRestoreReadFailureAfterLookup(t *testing.T) {
	for _, kind := range []string{"commit", "tree"} {
		t.Run(kind, func(t *testing.T) {
			repo := initTestRepo(t)
			writeFile(t, repo, "notes", "saved contents")
			store := NewSnapshotStore(repo)
			snap, err := store.Capture(t.Context(), "MGIT-9.2", time.Unix(1700000000, 0))
			require.NoError(t, err)
			hash := snap.CommitHash
			if kind == "tree" {
				hash = snap.TreeHash
			}

			dest := filepath.Join(t.TempDir(), "restore")
			cause := errors.New("snapshot object unavailable")
			healthy := repo.repo.Storer
			repo.repo.Storer = &snapshotRestoreReadFault{Storer: healthy, hash: plumbing.NewHash(hash), err: cause}
			n, err := store.Materialize(t.Context(), snap.ID, dest)
			repo.repo.Storer = healthy
			require.ErrorIs(t, err, cause)
			require.ErrorContains(t, err, "snapshot restore: read "+kind)
			require.Zero(t, n)
			_, err = os.Lstat(dest)
			require.ErrorIs(t, err, os.ErrNotExist)

			n, err = store.Materialize(t.Context(), snap.ID, dest)
			require.NoError(t, err)
			require.Equal(t, snap.FileCount, n)
			data, err := fs.ReadFile(os.DirFS(dest), "notes")
			require.NoError(t, err)
			require.Equal(t, "saved contents", string(data))
		})
	}
}
