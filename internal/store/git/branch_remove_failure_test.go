package git

import (
	"errors"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/storage"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

type interruptedRefRemoval struct {
	storage.Storer
	failures []error
}

func (s *interruptedRefRemoval) RemoveReference(ref plumbing.ReferenceName) error {
	if len(s.failures) > 0 {
		err := s.failures[0]
		s.failures = s.failures[1:]
		return err
	}
	return s.Storer.RemoveReference(ref)
}

func TestBranchRemovalReflectsFinalStorageOutcome(t *testing.T) {
	for _, tc := range []struct {
		name     string
		failures int
		removed  bool
	}{
		{"storage recovers", 1, true},
		{"storage remains unavailable", 2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, _, tip := faultRepoWithCommit(t)
			ctx := t.Context()
			bs := NewBranchStore(repo)
			first := errors.New("temporary reference store failure")
			last := errors.New("reference store still unavailable")
			original := repo.repo.Storer
			repo.repo.Storer = &interruptedRefRemoval{Storer: original, failures: []error{first, last}[:tc.failures]}
			err := bs.DeleteBranch(ctx, "task/MGIT-1", true)
			repo.repo.Storer = original
			if tc.removed {
				require.NoError(t, err)
				_, err = bs.GetBranch(ctx, "task/MGIT-1")
				require.ErrorIs(t, err, model.ErrBranchNotFound)
			} else {
				require.ErrorIs(t, err, last)
				branch, err := bs.GetBranch(ctx, "task/MGIT-1")
				require.NoError(t, err)
				require.Equal(t, tip, branch.HeadCommit)
			}
			head, err := repo.Head()
			require.NoError(t, err)
			require.Equal(t, tip, head)
			_, err = NewCommitStore(repo).GetCommit(ctx, tip)
			require.NoError(t, err, "removing a branch must not remove its commit")
		})
	}
}
