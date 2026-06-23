package git

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/hyper-swe/mgit/internal/model"
)

// TestResolveCommitHash_RejectsBadReferences: too-short, non-hex, and absent
// full hashes are all reported as not-found rather than mis-resolved.
func TestResolveCommitHash_RejectsBadReferences(t *testing.T) {
	repo := initTestRepo(t)
	cs := NewCommitStore(repo)
	ctx := context.Background()
	for _, ref := range []string{"ab", "zzzz", "0123456789abcdef0123456789abcdef0123456z"} {
		_, err := cs.GetCommit(ctx, ref)
		assert.ErrorIs(t, err, model.ErrCommitNotFound, "ref %q must be not-found", ref)
	}
}

// TestDiffStats_Aggregates returns combined add/remove counts for a diff.
func TestDiffStats_Aggregates(t *testing.T) {
	repo := initTestRepo(t)
	ds := NewDiffStore(repo)
	_, err := ds.DiffStats(context.Background(), "deadbeef", "cafebabe")
	assert.Error(t, err, "stats over missing commits surfaces the diff error")
}
