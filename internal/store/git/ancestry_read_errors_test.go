package git

import (
	"errors"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/require"
)

func TestIsAncestorOfHeadReportsUnreadableHistory(t *testing.T) {
	repo, store := newFaultRepo(t)
	commit := func(contents string) string {
		return writeAndCommit(t, repo, "MGIT-1", map[string]string{"notes": contents})
	}
	ancestor, middle, head := commit("first"), commit("second"), commit("third")
	for _, tc := range []struct{ name, hash, context string }{
		{"reference", "", "resolve HEAD"},
		{"candidate", ancestor, "load commit " + ancestor},
		{"branch tip", head, "load HEAD commit"},
		{"intermediate parent", middle, "ancestry check " + ancestor},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.hash == "" {
				store.failGetReference = true
			} else {
				store.failHash = plumbing.NewHash(tc.hash)
			}
			got, err := repo.IsAncestorOfHead(ancestor)
			store.failGetReference = false
			store.failHash = plumbing.ZeroHash
			require.False(t, got)
			require.ErrorContains(t, err, tc.context)
			require.Error(t, errors.Unwrap(err), "storage failure must remain available to callers")
			got, err = repo.IsAncestorOfHead(ancestor)
			require.NoError(t, err)
			require.True(t, got, "restoring reads must recover the original ancestry result")
		})
	}
}
