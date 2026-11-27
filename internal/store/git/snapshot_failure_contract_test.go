package git

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/storage"
	"github.com/stretchr/testify/require"
)

// Fail just one stage of publishing a snapshot; earlier writes still succeed.
type snapshotWriteFault struct {
	storage.Storer
	kind plumbing.ObjectType
	ref  bool
	err  error
}

func (s snapshotWriteFault) SetEncodedObject(o plumbing.EncodedObject) (plumbing.Hash, error) {
	if o.Type() == s.kind {
		return plumbing.ZeroHash, s.err
	}
	return s.Storer.SetEncodedObject(o)
}

func (s snapshotWriteFault) SetReference(r *plumbing.Reference) error {
	if s.ref {
		return s.err
	}
	return s.Storer.SetReference(r)
}

func TestSnapshotWriteFailureDoesNotPublishOrConsumeStaging(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind plumbing.ObjectType
		ref  bool
	}{
		{"blob", plumbing.BlobObject, false}, {"tree", plumbing.TreeObject, false},
		{"commit", plumbing.CommitObject, false}, {"reference", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := initTestRepo(t)
			ss, ctx := NewSnapshotStore(repo), t.Context()
			head, err := repo.Head()
			require.NoError(t, err)
			writeFile(t, repo, "notes", "first version")
			at := time.Unix(1700000000, 0)
			first, err := ss.Capture(ctx, "MGIT-9.2", at)
			require.NoError(t, err)
			writeFile(t, repo, "notes", "second version")
			require.NoError(t, NewWorktreeStore(repo).Add(ctx, "notes"))
			store := repo.repo.Storer
			fault := errors.New("snapshot storage unavailable")
			repo.repo.Storer = snapshotWriteFault{store, tc.kind, tc.ref, fault}
			failed, err := ss.Capture(ctx, "MGIT-9.2", at.Add(time.Second))
			repo.repo.Storer = store
			require.ErrorIs(t, err, fault)
			require.Nil(t, failed)
			listed, err := ss.List(ctx, "MGIT-9.2")
			require.NoError(t, err)
			require.Len(t, listed, 1)
			require.Equal(t, first.ID, listed[0].ID)
			staged, err := repo.StagedSnapshot()
			require.NoError(t, err)
			require.Equal(t, []string{"notes"}, staged)
			got, err := repo.Head()
			require.NoError(t, err)
			require.Equal(t, head, got)
		})
	}
}

func TestSnapshotReadFailureDoesNotPruneOrRestore(t *testing.T) {
	repo, fs, _ := faultRepoWithCommit(t)
	ss, ctx := NewSnapshotStore(repo), t.Context()
	snap, err := ss.Capture(ctx, "MGIT-9.2", time.Unix(1700000000, 0))
	require.NoError(t, err)
	fs.failHash = plumbing.NewHash(snap.CommitHash)
	dest := filepath.Join(t.TempDir(), "restore")
	n, err := ss.Materialize(ctx, snap.ID, dest)
	require.ErrorContains(t, err, snap.CommitHash)
	require.Zero(t, n)
	require.NoDirExists(t, dest)
	dropped, err := ss.Prune(ctx, "MGIT-9.2", 0)
	require.ErrorContains(t, err, snap.CommitHash)
	require.Zero(t, dropped)
	fs.failHash = plumbing.ZeroHash
	listed, err := ss.List(ctx, "MGIT-9.2")
	require.NoError(t, err)
	require.Len(t, listed, 1)
	require.Equal(t, snap.ID, listed[0].ID)
}
