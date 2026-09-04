package model

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// controlResponseLimit is controlproto.MaxResponseBytes.
//
// It is written here as its own literal rather than imported: internal/model
// must not depend on the transport (the dependency runs the other way), and
// duplicating the number is the point — if the transport's ceiling moves and
// this does not, the drift becomes a test failure instead of a report nobody
// can send. Refs: MGIT-160
const controlResponseLimit = 1 << 20 // controlproto.MaxResponseBytes

// pathsOfLength builds n paths of exactly width bytes each.
func pathsOfLength(n, width int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "src/" + strings.Repeat("x", width-4)
	}
	return out
}

func encodedSize(t *testing.T, r WorktreeSyncReport) int {
	t.Helper()
	b, err := json.Marshal(r)
	require.NoError(t, err)
	return len(b)
}

// THE GUARANTEE MGIT-160 IS SUPPOSED TO BUY: a bounded report fits the wire.
//
// The sizes are MEASURED through a real encode, not estimated. An arithmetic
// bound (500 x width x 4) would agree with itself while JSON escaping, field
// names and the envelope pushed the real payload over — and being over is the
// whole failure.
//
// The widths come from what a repository actually holds: git's own limit, a
// deep node_modules path, and a long generated one. Refs: MGIT-160
func TestBound_AtRealisticPathLengths_TheReportFitsTheWire(t *testing.T) {
	widths := map[string]int{
		"an_ordinary_source_path":  40,
		"a_deep_node_modules_path": 120,
		"a_long_generated_path":    255, // a single filename component's usual max
	}
	for name, width := range widths {
		t.Run(name, func(t *testing.T) {
			// Every class full, which is the worst case a real sync produces.
			r := WorktreeSyncReport{
				Updated:    pathsOfLength(40_000, width),
				Deleted:    pathsOfLength(5_000, width),
				Overridden: pathsOfLength(5_000, width),
				Conflicts:  conflictsOfLength(5_000, width),
			}

			size := encodedSize(t, r.Bound(SyncReportPathLimit))

			assert.Less(t, size, controlResponseLimit,
				"a bounded report must fit the control-response limit; encoded to %d bytes", size)
			t.Logf("%d-byte paths -> %d bytes encoded (%.0f%% of the limit)",
				width, size, 100*float64(size)/controlResponseLimit)
		})
	}
}

// WHERE THE GUARANTEE RUNS OUT, and what happens then.
//
// Bound caps the COUNT, not the bytes, so a tree of pathological path lengths
// can still exceed the wire limit after bounding. That is not a defect on its
// own — the transport refuses an oversize response legibly (MGIT-160 layer A)
// — but it is only not a defect if the two layers actually compose. This
// measures the crossover and asserts the fallback exists rather than assuming
// the cap is sufficient. Refs: MGIT-160
func TestBound_CapsTheCountNotTheBytes_AndTheTransportIsTheBackstop(t *testing.T) {
	// A path length no repository has, chosen to be unambiguously over.
	const pathological = 3000

	r := WorktreeSyncReport{Updated: pathsOfLength(40_000, pathological)}
	bounded := r.Bound(SyncReportPathLimit)

	size := encodedSize(t, bounded)
	t.Logf("%d paths of %d bytes -> %d bytes after bounding", SyncReportPathLimit, pathological, size)

	require.Greater(t, size, controlResponseLimit,
		"the premise: bounding by count cannot always bring the bytes under the limit")
	assert.Len(t, bounded.Updated, SyncReportPathLimit, "the cap still applied")
	assert.True(t, bounded.Truncated,
		"and the report still says it was shortened, so nothing here reads as complete")
	assert.Equal(t, 40_000, bounded.UpdatedTotal,
		"the honest total survives even when the payload cannot be sent")
	// The transport's refusal is asserted in internal/sandboxd's
	// oversize_response_test.go; what this pins is that the count cap alone is
	// NOT the guarantee, so removing that refusal would not be safe.
}

// EXPECTED TO FAIL — SKIPPED, NAMING MGIT-173.
//
// Bound's own doc comment says it lives on the model so "every producer and
// transport of a report inherits the same guarantee — and a new caller cannot
// forget it". A new caller cannot forget to bound; a new caller CAN bound
// something already bounded, and the guarantee then inverts:
//
//	once : UpdatedTotal=40000 DeletedTotal=700 ConflictsTotal=600 Truncated=true
//	twice: UpdatedTotal=500   DeletedTotal=500 ConflictsTotal=500 Truncated=true
//
// An 80x under-report of the number this field exists to carry; two totals
// capped that were never truncated at all; and a self-contradictory report
// claiming Truncated with a total equal to its own list length.
// Refs: MGIT-173, MGIT-160
func TestBound_IsIdempotent(t *testing.T) {

	r := WorktreeSyncReport{
		Updated:   pathsOfLength(40_000, 40),
		Deleted:   pathsOfLength(700, 40),
		Conflicts: conflictsOfLength(600, 40),
	}

	once := r.Bound(SyncReportPathLimit)
	twice := once.Bound(SyncReportPathLimit)

	assert.Equal(t, once, twice,
		"bounding a bounded report must change nothing; a second pass that recomputed "+
			"the totals from the capped lists would report 500 where 40,000 diverged")
	assert.Equal(t, 40_000, twice.UpdatedTotal)
	assert.True(t, twice.Truncated)
}

// One list's truncation must never be cleared by a later, shorter one. The
// flag is a property of the REPORT, not of the last list examined, and an
// unmarked report is defined to mean "nothing was dropped".
//
// The classes are permuted so the assertion cannot pass by accident of
// ordering — with one long list and three short ones, an implementation that
// overwrites rather than accumulates fails only when the long list is not
// last. Refs: MGIT-160
func TestBound_TruncationIsNeverClearedByALaterList(t *testing.T) {
	long := pathsOfLength(SyncReportPathLimit+1, 40)
	short := pathsOfLength(3, 40)

	tests := map[string]WorktreeSyncReport{
		"updated_is_the_long_one":    {Updated: long, Deleted: short, Overridden: short},
		"deleted_is_the_long_one":    {Updated: short, Deleted: long, Overridden: short},
		"overridden_is_the_long_one": {Updated: short, Deleted: short, Overridden: long},
		"conflicts_is_the_long_one": {
			Updated: short, Deleted: short, Overridden: short,
			Conflicts: conflictsOfLength(SyncReportPathLimit+1, 40),
		},
	}
	for name, r := range tests {
		t.Run(name, func(t *testing.T) {
			bounded := r.Bound(SyncReportPathLimit)

			assert.True(t, bounded.Truncated,
				"whichever class overflowed, the report must say it was shortened")
		})
	}
}

// A report entirely within the cap is unmarked — and an unmarked report is
// defined to mean exactly one thing: nothing was dropped. Every class is at
// the cap EXACTLY, which is the boundary an off-by-one lives on.
// Refs: MGIT-160
func TestBound_ExactlyAtTheCap_IsCompleteAndSaysSo(t *testing.T) {
	atCap := pathsOfLength(SyncReportPathLimit, 40)
	r := WorktreeSyncReport{
		Updated: atCap, Deleted: atCap, Overridden: atCap,
		Conflicts: conflictsOfLength(SyncReportPathLimit, 40),
	}

	bounded := r.Bound(SyncReportPathLimit)

	assert.False(t, bounded.Truncated,
		"a list exactly at the cap was not shortened, and must not claim to have been")
	assert.Len(t, bounded.Updated, SyncReportPathLimit)
	assert.Equal(t, SyncReportPathLimit, bounded.UpdatedTotal)

	oneMore := WorktreeSyncReport{Updated: pathsOfLength(SyncReportPathLimit+1, 40)}
	assert.True(t, oneMore.Bound(SyncReportPathLimit).Truncated,
		"one over the cap IS shortened, and must say so")
}

// The totals are always populated, truncated or not, so a reader never has to
// infer a count from a list length — the inference that produced "0 updated"
// beside a list of updates. Refs: MGIT-160
func TestBound_TotalsArePopulatedWhetherOrNotAnythingWasDropped(t *testing.T) {
	tests := map[string]WorktreeSyncReport{
		"a_small_report": {
			Updated: pathsOfLength(2, 40), Deleted: pathsOfLength(1, 40),
			Overridden: pathsOfLength(3, 40), Conflicts: conflictsOfLength(4, 40),
		},
		"a_large_report": {
			Updated: pathsOfLength(9_000, 40), Deleted: pathsOfLength(8_000, 40),
			Overridden: pathsOfLength(7_000, 40), Conflicts: conflictsOfLength(6_000, 40),
		},
	}
	for name, r := range tests {
		t.Run(name, func(t *testing.T) {
			b := r.Bound(SyncReportPathLimit)

			assert.Equal(t, len(r.Updated), b.UpdatedTotal)
			assert.Equal(t, len(r.Deleted), b.DeletedTotal)
			assert.Equal(t, len(r.Overridden), b.OverriddenTotal)
			assert.Equal(t, len(r.Conflicts), b.ConflictsTotal)
		})
	}
}

// Every non-path field survives bounding. Refused and Skipped in particular:
// a refusal lost to truncation would report a sync that did not happen as one
// that did. Refs: MGIT-160, MGIT-76
func TestBound_NoNonPathFieldIsLost(t *testing.T) {
	r := WorktreeSyncReport{
		Updated: pathsOfLength(40_000, 40),
		Skipped: true, DryRun: true, Refused: true,
		Detail: "the sandbox has not booted yet",
	}

	b := r.Bound(SyncReportPathLimit)

	assert.True(t, b.Skipped)
	assert.True(t, b.DryRun)
	assert.True(t, b.Refused, "a refusal must never be lost to truncation")
	assert.Equal(t, r.Detail, b.Detail)
}

// A non-positive limit means "do not shorten", and must still populate the
// totals — a caller that opted out of capping still needs honest counts, and
// must not receive a Truncated flag for a list nothing shortened.
func TestBound_ANonPositiveLimit_CountsWithoutShortening(t *testing.T) {
	r := WorktreeSyncReport{Updated: pathsOfLength(1_000, 40)}

	for _, limit := range []int{0, -1} {
		b := r.Bound(limit)

		assert.Len(t, b.Updated, 1_000, "limit %d must not shorten", limit)
		assert.Equal(t, 1_000, b.UpdatedTotal, "limit %d must still count", limit)
		assert.False(t, b.Truncated, "limit %d dropped nothing and must not claim to", limit)
	}
}

// Bound must not mutate its receiver's slices. The dispatch holds the full
// report for the audit record after bounding the wire copy; a Bound that
// aliased and truncated in place would shorten the record too.
// Refs: MGIT-160
func TestBound_DoesNotShortenTheCallersOwnReport(t *testing.T) {
	r := WorktreeSyncReport{Updated: pathsOfLength(40_000, 40)}

	_ = r.Bound(SyncReportPathLimit)

	assert.Len(t, r.Updated, 40_000,
		"the caller's report must still hold everything it found")
}

// Changed() answers from the lists, so a bounded report must still be able to
// say whether anything happened.
func TestBound_ABoundedReportStillKnowsWhetherAnythingChanged(t *testing.T) {
	r := WorktreeSyncReport{Updated: pathsOfLength(40_000, 40)}

	assert.True(t, r.Bound(SyncReportPathLimit).Changed())
	assert.False(t, WorktreeSyncReport{Skipped: true}.Bound(SyncReportPathLimit).Changed())
}

func conflictsOfLength(n, width int) []WorktreeSyncConflict {
	out := make([]WorktreeSyncConflict, n)
	paths := pathsOfLength(n, width)
	for i := range out {
		out[i] = WorktreeSyncConflict{Path: paths[i], Reason: "modified in the guest since it was delivered"}
	}
	return out
}

// The second pass's OTHER wrong, stated separately because it is a different
// defect from the under-report: a list that was never truncated must keep its
// true count. 700 becoming 500 is not a shortened list being described
// honestly — it is a count that was never true of anything.
// Refs: MGIT-173, MGIT-160
func TestBound_ASecondPass_DoesNotInventACapForListsUnderIt(t *testing.T) {

	r := WorktreeSyncReport{
		Updated: pathsOfLength(40_000, 40),
		Deleted: pathsOfLength(700, 40), // under the cap: never shortened
	}

	twice := r.Bound(SyncReportPathLimit).Bound(SyncReportPathLimit)

	assert.Equal(t, 700, twice.DeletedTotal,
		"a list the cap never touched must keep the count it actually had")
}

// The invariant that makes the two skipped tests above checkable without
// reading them: Truncated and "the total equals the list length" cannot both
// hold. No real sync produces that state, so any report carrying it has been
// through a bookkeeping bug.
//
// Asserted on a SINGLE bounding, which is the shipped path — so this one is
// not skipped and guards the fix from the other direction.
// Refs: MGIT-173, MGIT-160
func TestBound_TruncatedNeverCoexistsWithATotalEqualToTheListLength(t *testing.T) {
	tests := map[string]WorktreeSyncReport{
		"well_over_the_cap":  {Updated: pathsOfLength(40_000, 40)},
		"one_over_the_cap":   {Updated: pathsOfLength(SyncReportPathLimit+1, 40)},
		"exactly_at_the_cap": {Updated: pathsOfLength(SyncReportPathLimit, 40)},
		"well_under_the_cap": {Updated: pathsOfLength(7, 40)},
	}
	for name, r := range tests {
		t.Run(name, func(t *testing.T) {
			b := r.Bound(SyncReportPathLimit)

			if b.Truncated {
				assert.Greater(t, b.UpdatedTotal, len(b.Updated),
					"a shortened listing must report MORE than it shows, or it is not shortened")
				return
			}
			assert.Equal(t, len(b.Updated), b.UpdatedTotal,
				"an unmarked report must mean the list IS the whole answer")
		})
	}
}
