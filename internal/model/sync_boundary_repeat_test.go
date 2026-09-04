package model

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// A bounded report must be a fixed point of Bound, and its totals must stay
// consistent with its flag: Truncated is true exactly when some total exceeds
// its own list. The rows are the shapes a real sync produces — which lists
// exceed the cap — not anything derived from Bound's own arithmetic.
// Refs: MGIT-173, MGIT-160
func TestBound_SecondPass_KeepsEveryTrueCountAndTheFlagsMeaning(t *testing.T) {
	const cap = 5
	tests := []struct {
		name           string
		updated        int
		deleted        int
		conflicts      int
		wantTruncated  bool
		wantUpdatedTot int
		wantDeletedTot int
	}{
		{"nothing_over_the_cap", 3, 2, 1, false, 3, 2},
		{"only_updated_over_the_cap", 40, 2, 1, true, 40, 2},
		{"only_deleted_over_the_cap", 3, 9, 0, true, 3, 9},
		{"only_conflicts_over_the_cap", 0, 0, 7, true, 0, 0},
		{"everything_over_the_cap", 40, 7, 6, true, 40, 7},
		{"exactly_at_the_cap", 5, 5, 5, false, 5, 5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := WorktreeSyncReport{
				Updated:   pathsOfLength(tt.updated, 8),
				Deleted:   pathsOfLength(tt.deleted, 8),
				Conflicts: conflictsOfLength(tt.conflicts, 8),
			}
			once := r.Bound(cap)
			twice := once.Bound(cap)

			assert.Equal(t, once, twice, "Bound must be idempotent on the whole struct")
			assert.Equal(t, tt.wantTruncated, twice.Truncated)
			assert.Equal(t, tt.wantUpdatedTot, twice.UpdatedTotal, "a list under the cap keeps its true count")
			assert.Equal(t, tt.wantDeletedTot, twice.DeletedTotal)
			assert.Equal(t, tt.conflicts, twice.ConflictsTotal)
			assertFlagMatchesTotals(t, twice)
		})
	}
}

// assertFlagMatchesTotals is the invariant no real sync violates: Truncated
// holds if and only if some total is larger than the list that carries it.
func assertFlagMatchesTotals(t *testing.T, r WorktreeSyncReport) {
	t.Helper()
	dropped := r.UpdatedTotal > len(r.Updated) || r.DeletedTotal > len(r.Deleted) ||
		r.OverriddenTotal > len(r.Overridden) || r.ConflictsTotal > len(r.Conflicts)
	assert.Equal(t, dropped, r.Truncated,
		"Truncated=%v but the totals say dropped=%v: %d/%d updated, %d/%d deleted, %d/%d conflicts",
		r.Truncated, dropped, len(r.Updated), r.UpdatedTotal, len(r.Deleted), r.DeletedTotal,
		len(r.Conflicts), r.ConflictsTotal)
	assert.GreaterOrEqual(t, r.UpdatedTotal, len(r.Updated), "a total can never be below its own list")
	assert.GreaterOrEqual(t, r.DeletedTotal, len(r.Deleted))
	assert.GreaterOrEqual(t, r.ConflictsTotal, len(r.Conflicts))
}
