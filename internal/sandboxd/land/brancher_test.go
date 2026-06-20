package land

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gitstore "github.com/hyper-swe/mgit/internal/store/git"
)

// The real host MergeStore must satisfy the brancher's fast-forwarder seam.
var _ fastForwarder = (*gitstore.MergeStore)(nil)

// recordFF records the branch name + hash it was asked to fast-forward.
type recordFF struct {
	branch, hash string
	err          error
}

func (f *recordFF) FastForward(_ context.Context, branch, hash string) error {
	f.branch, f.hash = branch, hash
	return f.err
}

// TestStoreBrancher_FastForwardsTaskBranch verifies the brancher resolves
// the task to its canonical branch (task/<id>) and forwards that ref —
// the same convention branch creation/rollback use. Refs: FR-17.5, FR-5
func TestStoreBrancher_FastForwardsTaskBranch(t *testing.T) {
	ff := &recordFF{}
	require.NoError(t, NewStoreBrancher(ff).FastForward(context.Background(), "MGIT-1.2.3", "abc123"))
	assert.Equal(t, "task/MGIT-1.2.3", ff.branch, "the task's canonical branch is forwarded")
	assert.Equal(t, "abc123", ff.hash)
}

// TestStoreBrancher_ErrorSurfaces verifies a fast-forward failure (e.g. a
// non-fast-forward or missing branch) propagates.
func TestStoreBrancher_ErrorSurfaces(t *testing.T) {
	err := NewStoreBrancher(&recordFF{err: assert.AnError}).FastForward(context.Background(), "T", "h")
	assert.Error(t, err)
}
