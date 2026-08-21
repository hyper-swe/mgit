package model

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func paths(n int, prefix string) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%s/pkg-%d/dist/index.js", prefix, i)
	}
	return out
}

// A classification that can only answer for SMALL trees is not a
// classification. The report is bounded by construction so the answer fits
// whatever the tree looks like — a worktree with a host-side node_modules
// included, which is what took the whole verb down. Refs: MGIT-160
func TestWorktreeSyncReport_Bound_KeepsTheAnswerSendable(t *testing.T) {
	r := WorktreeSyncReport{
		Updated: paths(40_000, "node_modules/@scope"),
		Deleted: paths(5_000, "build"),
	}

	bounded := r.Bound(SyncReportPathLimit)

	encoded, err := json.Marshal(bounded)
	require.NoError(t, err)
	assert.Less(t, len(encoded), 1<<20,
		"a bounded report must fit the control-response limit, whatever the tree held")
}

// Truncation must be VISIBLE. A silently shortened list of diverged paths
// would be believed — which is worse than the crash it replaces, because a
// caller acting on "these 500 paths differ" when 40,000 do has been misled by
// its own tooling. Refs: MGIT-160, D-R9
func TestWorktreeSyncReport_Bound_MarksWhatItDropped(t *testing.T) {
	r := WorktreeSyncReport{Updated: paths(40_000, "node_modules"), Deleted: paths(3, "build")}

	bounded := r.Bound(SyncReportPathLimit)

	assert.Len(t, bounded.Updated, SyncReportPathLimit, "the list is capped")
	assert.True(t, bounded.Truncated, "a shortened report must say so")
	assert.Equal(t, 40_000, bounded.UpdatedTotal, "the FULL count survives truncation")
	assert.Equal(t, 3, bounded.DeletedTotal)
	assert.Len(t, bounded.Deleted, 3, "a list that fits is not touched")
}

// A report that already fits is returned unchanged and unmarked, so a reader
// can trust an unmarked report completely. Refs: MGIT-160
func TestWorktreeSyncReport_Bound_SmallReportIsUntouched(t *testing.T) {
	r := WorktreeSyncReport{Updated: []string{"a.txt", "b.txt"}, Deleted: []string{"c.txt"}, DryRun: true}

	bounded := r.Bound(SyncReportPathLimit)

	assert.False(t, bounded.Truncated, "an unmarked report must mean nothing was dropped")
	assert.Equal(t, r.Updated, bounded.Updated)
	assert.Equal(t, r.Deleted, bounded.Deleted)
	assert.True(t, bounded.DryRun, "every non-path field survives")
	assert.Equal(t, 2, bounded.UpdatedTotal, "totals are always populated, truncated or not")
}

// Conflicts are the paths a caller most needs, and they are the ones a refusal
// is built from — so they are bounded too, and counted honestly.
// Refs: MGIT-160, MGIT-76
func TestWorktreeSyncReport_Bound_ConflictsAreBoundedAndCounted(t *testing.T) {
	conflicts := make([]WorktreeSyncConflict, 20_000)
	for i := range conflicts {
		conflicts[i] = WorktreeSyncConflict{Path: fmt.Sprintf("src/file-%d.ts", i)}
	}
	r := WorktreeSyncReport{Conflicts: conflicts, Refused: true}

	bounded := r.Bound(SyncReportPathLimit)

	assert.Len(t, bounded.Conflicts, SyncReportPathLimit)
	assert.Equal(t, 20_000, bounded.ConflictsTotal)
	assert.True(t, bounded.Truncated)
	assert.True(t, bounded.Refused, "the refusal itself is never lost to truncation")
}
