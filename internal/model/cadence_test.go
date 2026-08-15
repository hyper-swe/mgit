package model_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// at parses one RFC3339 instant, for readable fixtures built from the real
// timestamps this repo's index holds.
func at(t *testing.T, s string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, s)
	require.NoError(t, err)
	return parsed
}

// times parses a run of RFC3339 instants into an ascending commit trail.
func times(t *testing.T, ss ...string) []time.Time {
	t.Helper()
	out := make([]time.Time, 0, len(ss))
	for _, s := range ss {
		out = append(out, at(t, s))
	}
	return out
}

// TestClassifyCadence_MeasuredRealShapes_MatchTheFinding is the ground-truth
// test. Every fixture below is a VERBATIM copy of what this repo's index holds
// for that task: the worktree's created_at from the worktrees registry, and
// the authored task_commits timestamps with the derived squash artifact
// removed. If the classifier and the MGIT-110 finding ever disagree, one of
// them is wrong and this test is where that shows up.
// Refs: MGIT-110, R-H234, FR-4
func TestClassifyCadence_MeasuredRealShapes_MatchTheFinding(t *testing.T) {
	tests := []struct {
		name        string
		runStart    string
		commits     []string
		wantVerdict string
		wantReason  string
	}{
		{
			// Seven index rows; six authored, the seventh is the squash. All
			// six inside the last 5 minutes of a ~40-minute run.
			name:     "MGIT-102_end_burst",
			runStart: "2026-08-12T11:48:35Z",
			commits: []string{
				"2026-08-12T12:23:40Z", "2026-08-12T12:23:53Z", "2026-08-12T12:24:04Z",
				"2026-08-12T12:24:15Z", "2026-08-12T12:24:25Z", "2026-08-12T12:28:21Z",
			},
			wantVerdict: model.CadencePackagedPostHoc,
		},
		{
			// The finding counted 2 commits; one of those was the squash
			// artifact. One authored commit closing a ~33-minute run is not a
			// measurement that could not be made — it is the COMPLETE
			// observation that the run left no process history behind it.
			name:        "MGIT-103_single_checkpoint",
			runStart:    "2026-08-12T17:39:31Z",
			commits:     []string{"2026-08-12T18:12:22Z"},
			wantVerdict: model.CadenceSingleCheckpoint,
		},
		{
			name:        "MGIT-105_single_checkpoint",
			runStart:    "2026-08-12T17:39:31Z",
			commits:     []string{"2026-08-12T17:55:56Z"},
			wantVerdict: model.CadenceSingleCheckpoint,
		},
		{
			// The interrupted run. Zero commits is a COMPLETE fact, not a
			// missing measurement — it gets its own verdict, never a refusal.
			name:        "MGIT-109_no_commits_at_all",
			runStart:    "2026-08-12T18:22:38Z",
			commits:     nil,
			wantVerdict: model.CadenceNoCommits,
		},
		{
			// The contrast case: five authored commits over 23 minutes of a
			// 31-minute run. The label has to tell this apart from MGIT-102.
			name:     "MGIT-112_spread_across_run",
			runStart: "2026-08-15T10:52:50Z",
			commits: []string{
				"2026-08-15T11:01:23Z", "2026-08-15T11:06:48Z", "2026-08-15T11:13:58Z",
				"2026-08-15T11:22:56Z", "2026-08-15T11:24:07Z",
			},
			wantVerdict: model.CadenceSpreadAcrossRun,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := model.ClassifyCadence(model.CadenceObservation{
				RunStart:    at(t, tt.runStart),
				HasRunStart: true,
				CommitTimes: times(t, tt.commits...),
			})

			assert.Equal(t, tt.wantVerdict, got.Verdict)
			assert.Equal(t, tt.wantReason, got.Reason)
			assert.Equal(t, len(tt.commits), got.Commits)
			assert.NotEmpty(t, got.Summary, "a human still needs the sentence")
		})
	}
}

// TestClassifyCadence_OneCommitOnATrustedDenominator_IsItsOwnObservation.
//
// Pooling this into INSUFFICIENT_EVIDENCE hid the MOST COMMON shape: across
// the six measured runs on this repo, manufactured trails are ONE of six and
// half the runs produced a single commit or none. "Cannot tell" is the wrong
// thing to say about one commit closing a 33-minute run — that is not a
// measurement that failed, it is a complete observation that the run has no
// process history, and it is the same argument NO_COMMITS already won, one
// commit later.
//
// What makes it tractable is the denominator: a single commit is only
// remarkable in PROPORTION to the run it closes, so this verdict is gated on
// exactly the elapsed-time trust every other verdict is.
// Refs: MGIT-110, R-H234
func TestClassifyCadence_OneCommitOnATrustedDenominator_IsItsOwnObservation(t *testing.T) {
	tests := []struct {
		name        string
		runStart    string
		commit      string
		wantVerdict string
		wantReason  string
	}{
		{
			// MGIT-103's shape at measurement time: ~33 minutes, one commit.
			name:        "one_commit_closing_a_long_run",
			runStart:    "2026-08-12T17:39:31Z",
			commit:      "2026-08-12T18:12:22Z",
			wantVerdict: model.CadenceSingleCheckpoint,
		},
		{
			// Proportionate and unremarkable: one commit closing a 3-minute
			// run is exactly what a 3-minute run should look like.
			name:        "one_commit_closing_a_three_minute_run",
			runStart:    "2026-08-15T09:00:00Z",
			commit:      "2026-08-15T09:03:00Z",
			wantVerdict: model.CadenceInsufficientEvidence,
			wantReason:  model.CadenceReasonRunTooShort,
		},
		{
			// MGIT-109's shape TODAY: one recovery-session commit against a
			// worktree from the previous evening. The denominator is not
			// measuring a run, so no observation about the run is available.
			name:        "one_commit_on_a_multi_session_worktree",
			runStart:    "2026-08-12T18:22:38Z",
			commit:      "2026-08-13T03:57:43Z",
			wantVerdict: model.CadenceInsufficientEvidence,
			wantReason:  model.CadenceReasonRunTooLong,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := model.ClassifyCadence(model.CadenceObservation{
				RunStart:    at(t, tt.runStart),
				HasRunStart: true,
				CommitTimes: times(t, tt.commit),
			})

			assert.Equal(t, tt.wantVerdict, got.Verdict)
			assert.Equal(t, tt.wantReason, got.Reason)
			assert.Equal(t, 1, got.Commits)
		})
	}
}

// TestClassifyCadence_SingleCheckpoint_PublishesTheRunItClosed: the whole
// point is the PROPORTION, so the elapsed time a reviewer needs to judge it
// travels with the verdict. The window is genuinely zero here — one commit has
// no span — and that zero must not be mistaken for the burst fraction, which
// is why the verdict token, not the ratio, is what a consumer branches on.
// Refs: MGIT-110
func TestClassifyCadence_SingleCheckpoint_PublishesTheRunItClosed(t *testing.T) {
	got := model.ClassifyCadence(model.CadenceObservation{
		RunStart:    at(t, "2026-08-12T17:39:31Z"),
		HasRunStart: true,
		CommitTimes: times(t, "2026-08-12T18:12:22Z"),
	})

	require.Equal(t, model.CadenceSingleCheckpoint, got.Verdict)
	require.NotNil(t, got.Measure)
	assert.InDelta(t, 1971.0, got.Measure.ElapsedSeconds, 0.5, "~33 minutes")
	assert.Zero(t, got.Measure.WindowSeconds)
	assert.Equal(t, got.Measure.FirstCommitAt, got.Measure.LastCommitAt)
	assert.Contains(t, got.Summary, "32m51s", "the run it closed is stated, not judged")
}

// TestClassifyCadence_ShortRunTightWindow_IsNotCalledABurst guards the
// false positive that would discredit the whole label. Six minutes of commits
// is a burst across a forty-minute run and is COMPLETE COVERAGE of a
// six-minute one; the denominator, not the window, decides. A short run is
// refused outright rather than accused.
// Refs: MGIT-110, R-H234
func TestClassifyCadence_ShortRunTightWindow_IsNotCalledABurst(t *testing.T) {
	tests := []struct {
		name        string
		runStart    string
		commits     []string
		wantVerdict string
		wantReason  string
	}{
		{
			// 8-minute run, two commits 20 seconds apart. Tight window, but
			// the run is too short for "packaged post-hoc" to mean anything.
			name:        "eight_minute_run_tight_window",
			runStart:    "2026-08-15T09:00:00Z",
			commits:     []string{"2026-08-15T09:07:40Z", "2026-08-15T09:08:00Z"},
			wantVerdict: model.CadenceInsufficientEvidence,
			wantReason:  model.CadenceReasonRunTooShort,
		},
		{
			// The same window over a run just under the floor is still refused.
			name:        "just_under_the_floor",
			runStart:    "2026-08-15T09:00:00Z",
			commits:     []string{"2026-08-15T09:14:40Z", "2026-08-15T09:14:59Z"},
			wantVerdict: model.CadenceInsufficientEvidence,
			wantReason:  model.CadenceReasonRunTooShort,
		},
		{
			// Commits covering the whole of a short run are not a burst even
			// once the run is long enough to judge.
			name:        "complete_coverage_of_a_short_run",
			runStart:    "2026-08-15T09:00:00Z",
			commits:     []string{"2026-08-15T09:01:00Z", "2026-08-15T09:16:00Z"},
			wantVerdict: model.CadenceSpreadAcrossRun,
		},
		{
			// Borderline: a window just over a quarter of the run is called
			// SPREAD, not accused. The threshold leans toward not labeling.
			name:        "borderline_above_the_ratio_is_not_accused",
			runStart:    "2026-08-15T09:00:00Z",
			commits:     []string{"2026-08-15T09:29:00Z", "2026-08-15T09:40:00Z"},
			wantVerdict: model.CadenceSpreadAcrossRun,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := model.ClassifyCadence(model.CadenceObservation{
				RunStart:    at(t, tt.runStart),
				HasRunStart: true,
				CommitTimes: times(t, tt.commits...),
			})

			assert.Equal(t, tt.wantVerdict, got.Verdict)
			assert.Equal(t, tt.wantReason, got.Reason)
			assert.NotEqual(t, model.CadencePackagedPostHoc, got.Verdict,
				"a short task must never be accused of packaging")
		})
	}
}

// TestClassifyCadence_UntrustedDenominator_IsInsufficientEvidence covers every
// way the denominator stops measuring one agent run. In each case the WINDOW
// alone would read as a textbook burst; refusing is the whole point.
// Refs: MGIT-110, R-H234
func TestClassifyCadence_UntrustedDenominator_IsInsufficientEvidence(t *testing.T) {
	tests := []struct {
		name       string
		obs        func(t *testing.T) model.CadenceObservation
		wantReason string
	}{
		{
			// No `mgit work` worktree for this task: nothing says when the run
			// began, so there is no denominator at all.
			name: "no_worktree_registered",
			obs: func(t *testing.T) model.CadenceObservation {
				return model.CadenceObservation{
					HasRunStart: false,
					CommitTimes: times(t, "2026-08-15T09:40:00Z", "2026-08-15T09:40:13Z"),
				}
			},
			wantReason: model.CadenceReasonNoDenominator,
		},
		{
			// MGIT-103's real shape TODAY: a worktree from the previous
			// evening with a second session's commits on it. Elapsed reads as
			// 11 hours, which is not one agent run, so no verdict is honest.
			name: "trail_spans_more_than_one_run",
			obs: func(t *testing.T) model.CadenceObservation {
				return model.CadenceObservation{
					RunStart:    at(t, "2026-08-12T17:39:31Z"),
					HasRunStart: true,
					CommitTimes: times(t,
						"2026-08-12T18:12:22Z", "2026-08-13T04:15:22Z", "2026-08-13T04:37:59Z"),
				}
			},
			wantReason: model.CadenceReasonRunTooLong,
		},
		{
			// Commits older than the worktree that supposedly contains them:
			// clock skew, an imported trail, or a re-registered worktree.
			name: "commits_predate_the_worktree",
			obs: func(t *testing.T) model.CadenceObservation {
				return model.CadenceObservation{
					RunStart:    at(t, "2026-08-15T10:00:00Z"),
					HasRunStart: true,
					CommitTimes: times(t, "2026-08-15T09:00:00Z", "2026-08-15T10:40:00Z"),
				}
			},
			wantReason: model.CadenceReasonCommitsPrecedeWorktree,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := model.ClassifyCadence(tt.obs(t))

			assert.Equal(t, model.CadenceInsufficientEvidence, got.Verdict)
			assert.Equal(t, tt.wantReason, got.Reason)
			assert.Nil(t, got.Measure,
				"a measurement must not be published on a denominator we do not trust")
		})
	}
}

// TestClassifyCadence_DecidedVerdict_PublishesItsMeasurement: a verdict a
// reviewer is asked to act on must show its working — the run start, the
// window, and the fraction that produced it — so the reviewer can disagree
// with the arithmetic rather than only with the word.
// Refs: MGIT-110
func TestClassifyCadence_DecidedVerdict_PublishesItsMeasurement(t *testing.T) {
	got := model.ClassifyCadence(model.CadenceObservation{
		RunStart:    at(t, "2026-08-12T11:48:35Z"),
		HasRunStart: true,
		CommitTimes: times(t, "2026-08-12T12:23:40Z", "2026-08-12T12:28:21Z"),
	})

	require.Equal(t, model.CadencePackagedPostHoc, got.Verdict)
	require.NotNil(t, got.Measure)
	assert.Equal(t, model.CadenceDenominatorWorktree, got.Measure.Denominator)
	assert.Equal(t, "2026-08-12T11:48:35Z", got.Measure.RunStartAt)
	assert.Equal(t, "2026-08-12T12:23:40Z", got.Measure.FirstCommitAt)
	assert.Equal(t, "2026-08-12T12:28:21Z", got.Measure.LastCommitAt)
	assert.InDelta(t, 2386.0, got.Measure.ElapsedSeconds, 0.5)
	assert.InDelta(t, 281.0, got.Measure.WindowSeconds, 0.5)
	assert.InDelta(t, 0.1178, got.Measure.WindowFraction, 0.001)
}

// TestClassifyCadence_UnorderedCommitTimes_SortsBeforeMeasuring: the caller
// orders by `position`, which is not guaranteed to be chronological. A
// negative window would silently read as a burst.
// Refs: MGIT-110
func TestClassifyCadence_UnorderedCommitTimes_SortsBeforeMeasuring(t *testing.T) {
	ordered := times(t, "2026-08-15T11:01:23Z", "2026-08-15T11:24:07Z")
	shuffled := []time.Time{ordered[1], ordered[0]}

	got := model.ClassifyCadence(model.CadenceObservation{
		RunStart:    at(t, "2026-08-15T10:52:50Z"),
		HasRunStart: true,
		CommitTimes: shuffled,
	})

	require.NotNil(t, got.Measure)
	assert.Equal(t, "2026-08-15T11:01:23Z", got.Measure.FirstCommitAt)
	assert.Positive(t, got.Measure.WindowSeconds)
}

// TestCadenceTokens_ClosedSet_Golden pins the machine-readable contract.
//
// The tokens below are the contract: an integration branches on them and they
// do not change without a deliberate, reviewed edit to this list. The prose in
// Summary is explicitly NOT contract and is free to be reworded at any time —
// which is exactly why the tokens exist (the MGIT-109 precedent: a consumer
// that matched on wording missed the condition it was watching for).
//
// Adding a token here is a decision, not a side effect — SINGLE_CHECKPOINT
// was added by editing this list, and the SINGLE_COMMIT refusal reason was
// removed from it in the same change, because with a trusted denominator a
// lone commit is now an observation rather than a failure to observe, and with
// an untrusted one the denominator's own reason applies. A reason that can
// never be emitted is dead contract.
// Refs: MGIT-110, MGIT-109, R-H234
func TestCadenceTokens_ClosedSet_Golden(t *testing.T) {
	goldenVerdicts := []string{
		"PACKAGED_POST_HOC",
		"SPREAD_ACROSS_RUN",
		"SINGLE_CHECKPOINT",
		"NO_COMMITS",
		"INSUFFICIENT_EVIDENCE",
	}
	goldenReasons := []string{
		"NO_DENOMINATOR",
		"COMMITS_PRECEDE_WORKTREE",
		"RUN_TOO_LONG",
		"RUN_TOO_SHORT",
	}
	goldenDenominators := []string{
		"WORKTREE_CREATED_TO_LAST_COMMIT",
	}

	assert.Equal(t, goldenVerdicts, model.CadenceVerdicts(),
		"the verdict set is a published contract; changing it is a reviewed decision")
	assert.Equal(t, goldenReasons, model.CadenceReasons(),
		"the refusal reasons are a published contract")
	assert.Equal(t, goldenDenominators, model.CadenceDenominators(),
		"every denominator a verdict can rest on is named and closed")

	for _, v := range goldenVerdicts {
		assert.True(t, model.ValidCadenceVerdict(v), "%s must validate", v)
	}
	for _, r := range goldenReasons {
		assert.True(t, model.ValidCadenceReason(r), "%s must validate", r)
	}
	assert.False(t, model.ValidCadenceVerdict("BURST"), "a near-miss is not a member")
	assert.False(t, model.ValidCadenceVerdict(""), "empty is not a member")
	assert.False(t, model.ValidCadenceReason("UNKNOWN"), "the reason set has no catch-all")
}

// TestClassifyCadence_EveryOutcome_IsAMemberOfTheClosedSet holds the set
// closed from BOTH sides: the classifier may never invent a token, and every
// token in the published set must be reachable.
//
// The second half is not symmetry for its own sake. SINGLE_COMMIT survived in
// the contract after the SINGLE_CHECKPOINT verdict made it unemittable, and a
// documented token that nothing can ever produce is a promise to an
// integration that will never be kept. Refs: MGIT-110, MGIT-109
func TestClassifyCadence_EveryOutcome_IsAMemberOfTheClosedSet(t *testing.T) {
	observations := []model.CadenceObservation{
		{HasRunStart: false},
		{HasRunStart: false, CommitTimes: times(t, "2026-08-15T09:00:00Z")},
		{HasRunStart: false, CommitTimes: times(t, "2026-08-15T09:00:00Z", "2026-08-15T09:00:13Z")},
		{HasRunStart: true, RunStart: at(t, "2026-08-15T09:00:00Z")},
		{HasRunStart: true, RunStart: at(t, "2026-08-15T09:00:00Z"),
			CommitTimes: times(t, "2026-08-15T09:50:00Z")},
		{HasRunStart: true, RunStart: at(t, "2026-08-15T09:00:00Z"),
			CommitTimes: times(t, "2026-08-15T09:50:00Z", "2026-08-15T09:50:13Z")},
		{HasRunStart: true, RunStart: at(t, "2026-08-15T09:00:00Z"),
			CommitTimes: times(t, "2026-08-15T09:05:00Z", "2026-08-15T09:50:00Z")},
		{HasRunStart: true, RunStart: at(t, "2026-08-15T09:00:00Z"),
			CommitTimes: times(t, "2026-08-13T09:00:00Z", "2026-08-15T09:50:00Z")},
		{HasRunStart: true, RunStart: at(t, "2026-08-15T09:00:00Z"),
			CommitTimes: times(t, "2026-08-15T09:05:00Z", "2026-08-16T09:50:00Z")},
		{HasRunStart: true, RunStart: at(t, "2026-08-15T09:00:00Z"),
			CommitTimes: times(t, "2026-08-15T09:03:00Z")},
	}

	seenVerdicts, seenReasons := map[string]bool{}, map[string]bool{}
	for i, obs := range observations {
		ev := model.ClassifyCadence(obs)
		assert.True(t, model.ValidCadenceVerdict(ev.Verdict), "case %d verdict %q", i, ev.Verdict)
		seenVerdicts[ev.Verdict] = true
		if ev.Verdict == model.CadenceInsufficientEvidence {
			assert.True(t, model.ValidCadenceReason(ev.Reason),
				"case %d: a refusal must say which refusal it is", i)
			seenReasons[ev.Reason] = true
		} else {
			assert.Empty(t, ev.Reason, "case %d: only a refusal carries a reason", i)
		}
		assert.NotEmpty(t, ev.Summary, "case %d", i)
	}

	for _, v := range model.CadenceVerdicts() {
		assert.True(t, seenVerdicts[v], "verdict %s is published but nothing can produce it", v)
	}
	for _, r := range model.CadenceReasons() {
		assert.True(t, seenReasons[r], "reason %s is published but nothing can produce it", r)
	}
}

// TestIsDerivedCommitAuthor_SquashAndMerge_AreNotProcessHistory: a squash
// artifact is a restatement of commits that already exist, written at hand-off
// — counting it inflates the trail and drags the window to the end of the run,
// which flatters exactly the shape this label exists to expose. Rollback and
// cherry-pick are the opposite: they are decisions taken DURING the run and
// are genuine timestamped evidence of it.
// Refs: MGIT-110, MGIT-22
func TestIsDerivedCommitAuthor_SquashAndMerge_AreNotProcessHistory(t *testing.T) {
	tests := []struct {
		agentID string
		want    bool
	}{
		{agentID: "mgit-squash", want: true},
		{agentID: "mgit-merge", want: true},
		{agentID: "mgit-rollback", want: false},
		{agentID: "mgit-cherry-pick", want: false},
		{agentID: "mgit-sync", want: false},
		{agentID: "cli", want: false},
		{agentID: "claude-mgit-110", want: false},
		{agentID: "", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.agentID, func(t *testing.T) {
			assert.Equal(t, tt.want, model.IsDerivedCommitAuthor(tt.agentID))
		})
	}
}
