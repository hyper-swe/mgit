package model

import (
	"fmt"
	"sort"
	"time"
)

// Commit-cadence evidence tokens.
//
// These are the stable, machine-readable contract of the cadence label. An
// integration branches on THESE; the Summary prose is free to be reworded at
// any time and must never be matched on. The set is closed — see
// CadenceVerdicts and its golden test. Refs: MGIT-110, MGIT-109, R-H234
const (
	// CadencePackagedPostHoc: the task's commits were written inside a small
	// slice at the end of the run. The trail claims a sequence of coherent
	// steps; what happened was one step split N ways at hand-off time. This is
	// the founder's phrase in R-H234 — packaged post-hoc, not process history.
	CadencePackagedPostHoc = "PACKAGED_POST_HOC"
	// CadenceSpreadAcrossRun: the commits are distributed through the run.
	// This is CONSISTENT WITH process history; it is not proof of it. Nothing
	// stops an agent from spacing out a manufactured trail, so this token
	// reports an observation and confers no quality judgement.
	CadenceSpreadAcrossRun = "SPREAD_ACROSS_RUN"
	// CadenceSingleCheckpoint: the task recorded exactly one authored commit,
	// closing a run long enough for that to be worth saying. There is no
	// process history here — no earlier point in the run to return to — and
	// that is a COMPLETE observation, not a measurement that could not be
	// made. It is the same argument NO_COMMITS already wins, one commit later.
	//
	// It exists because pooling it into INSUFFICIENT_EVIDENCE hid the most
	// common shape: across the six measured runs on this repo, a manufactured
	// trail is one of six, while HALF the runs produced a single commit or
	// none. The verdict is gated on the same denominator trust as every other,
	// because a lone commit is only remarkable in proportion to the run it
	// closes — one commit ending a three-minute run is unremarkable, and is
	// refused as RUN_TOO_SHORT.
	CadenceSingleCheckpoint = "SINGLE_CHECKPOINT"
	// CadenceNoCommits: the task has no authored commits at all. This is a
	// COMPLETE fact, not a missing measurement, so it is never collapsed into
	// INSUFFICIENT_EVIDENCE — it is the MGIT-109 case, where an interrupted
	// agent left nothing behind and mgit contributed nothing to its recovery.
	CadenceNoCommits = "NO_COMMITS"
	// CadenceInsufficientEvidence: the record cannot support a verdict. The
	// default branch of this classifier is the humble one, ALWAYS: a wrong
	// accusation of forgery is worse than an absent label, and a default of
	// "fine" would be worse still. Always accompanied by a Reason.
	CadenceInsufficientEvidence = "INSUFFICIENT_EVIDENCE"
)

// Reasons a cadence verdict is refused. Every INSUFFICIENT_EVIDENCE result
// carries exactly one, so a reader learns WHICH thing could not be measured.
// There is deliberately no catch-all: an unclassifiable refusal would be a
// gap in this list, and should be added to it.
//
// Every member is a failure of the DENOMINATOR — the thing that stops one
// agent run from being measurable. Nothing about the commit COUNT belongs
// here: zero commits and one commit are both complete observations with
// verdicts of their own. Refs: MGIT-110
const (
	// CadenceReasonNoDenominator: no worktree is registered for this task, so
	// nothing in the store says when the run began. Commits made outside
	// `mgit work` land here.
	CadenceReasonNoDenominator = "NO_DENOMINATOR"
	// CadenceReasonCommitsPrecedeWorktree: a commit is older than the worktree
	// that supposedly holds it — clock skew, an imported trail, or a
	// re-registered path. The denominator is not measuring this work.
	CadenceReasonCommitsPrecedeWorktree = "COMMITS_PRECEDE_WORKTREE"
	// CadenceReasonRunTooLong: the span from worktree creation to the last
	// commit exceeds any plausible single agent run, so the worktree is being
	// reused across sessions (or sat idle). A tight window measured against a
	// multi-session denominator manufactures a false burst.
	CadenceReasonRunTooLong = "RUN_TOO_LONG"
	// CadenceReasonRunTooShort: the run is short enough that a tight commit
	// window is indistinguishable from complete coverage of it. Six minutes of
	// commits is a burst across a forty-minute run and is the WHOLE of a
	// six-minute one.
	CadenceReasonRunTooShort = "RUN_TOO_SHORT"
)

// CadenceDenominatorWorktree names the only denominator this build measures
// against: the worktree's created_at in the worktrees registry (written by
// `mgit work`) through to the last authored commit.
//
// What it actually measures: the interval between the moment a task-bound
// worktree was provisioned and the last time that task recorded work. It is a
// PROXY for the agent run, and the proxy is wrong in known ways —
// CadenceReasonRunTooLong and CadenceReasonCommitsPrecedeWorktree exist to
// catch the two that are detectable. The undetectable one: a worktree
// provisioned some minutes before the agent actually started inflates the
// denominator and makes the trail look more bunched than it was. That bias
// runs toward accusing, which is why the burst threshold is set conservatively
// and why short runs are refused outright.
//
// It also cannot see the run's END. The interval terminates at the last
// authored commit because that is the last thing the store knows about; the
// label therefore says nothing about work done after it. Refs: MGIT-110, FR-16
const CadenceDenominatorWorktree = "WORKTREE_CREATED_TO_LAST_COMMIT"

// Thresholds governing the cadence verdict. They are deliberately blunt and
// deliberately biased toward refusing to label. Refs: MGIT-110, R-H234
const (
	// CadenceBurstFraction is the largest share of the run a commit window can
	// occupy and still read as packaging. Measured shapes on this repo:
	// MGIT-102 (the end-burst) sits at 0.12, MGIT-112 (spread) at 0.73. A
	// quarter sits well below the observed gap on purpose — a borderline trail
	// is reported as SPREAD rather than accused of forgery.
	CadenceBurstFraction = 0.25
	// CadenceMinRun is the shortest run this classifier will judge. Below it,
	// a tight window is not evidence of anything.
	CadenceMinRun = 15 * time.Minute
	// CadenceMaxRun is the longest span still credible as ONE agent run. Real
	// sub-agent runs on this repo are tens of minutes; a multi-hour span means
	// the worktree outlived a session, and the denominator stopped measuring
	// a run. MGIT-103's registry row is the live example: a worktree from the
	// previous evening with a second session's commits on it.
	CadenceMaxRun = 4 * time.Hour
)

// derivedCommitAuthors are the agent IDs whose commits RESTATE work that
// already exists, at integration time, rather than recording it as it
// happened. They are excluded from the authored trail: counting a squash
// artifact both inflates the commit count and drags the window's end to
// hand-off time, which flatters the exact shape this label exists to expose.
//
// Rollback, cherry-pick and sync are deliberately NOT here. They write new
// content at the moment a decision was taken during the run, so they are
// genuine timestamped evidence of it. Refs: MGIT-110, MGIT-22
var derivedCommitAuthors = map[string]bool{
	"mgit-squash": true,
	"mgit-merge":  true,
}

// IsDerivedCommitAuthor reports whether commits by this agent ID are
// integration artifacts rather than process history. Refs: MGIT-110
func IsDerivedCommitAuthor(agentID string) bool {
	return derivedCommitAuthors[agentID]
}

// CadenceObservation is the raw input to ClassifyCadence: everything the
// store knows about when a task's work happened, and nothing about what it
// means. CommitTimes need not be sorted. Refs: MGIT-110
type CadenceObservation struct {
	// CommitTimes are the creation times of the task's AUTHORED commits.
	// Callers filter derived authors out first (IsDerivedCommitAuthor).
	CommitTimes []time.Time
	// RunStart is the denominator's origin — the worktree's created_at.
	RunStart time.Time
	// HasRunStart is false when no worktree is registered for the task, which
	// is a refusal, not a zero.
	HasRunStart bool
}

// CadenceMeasure is the arithmetic behind a decided verdict, published so a
// reviewer can disagree with the numbers rather than only with the word.
// It is absent whenever the verdict is a refusal. Refs: MGIT-110
type CadenceMeasure struct {
	Denominator    string  `json:"denominator"`
	RunStartAt     string  `json:"run_start_at"`
	FirstCommitAt  string  `json:"first_commit_at"`
	LastCommitAt   string  `json:"last_commit_at"`
	ElapsedSeconds float64 `json:"elapsed_seconds"`
	WindowSeconds  float64 `json:"window_seconds"`
	WindowFraction float64 `json:"window_fraction_of_run"`
}

// CadenceEvidence is the mechanical evidence label for one task's commit
// trail: what was observed, and what that does or does not support.
//
// It is EVIDENCE, not a score. There is no percentage, no grade and no
// ranking, and nothing in mgit gates on it — by design. An agent that commits
// to satisfy a checker produces exactly the manufactured trail this label
// exists to expose. Refs: MGIT-110, R-H234
type CadenceEvidence struct {
	// Verdict is one of the Cadence* verdict constants. Never empty.
	Verdict string `json:"verdict"`
	// Reason is one of the CadenceReason* constants, set if and only if
	// Verdict is INSUFFICIENT_EVIDENCE.
	Reason string `json:"reason,omitempty"`
	// Commits is the number of authored commits considered.
	Commits int `json:"commits"`
	// Measure is present only for a decided verdict.
	Measure *CadenceMeasure `json:"measure,omitempty"`
	// Summary is for the person reading the terminal. It is NOT contract:
	// match on Verdict and Reason, never on this. Refs: MGIT-109
	Summary string `json:"summary"`
}

// ClassifyCadence labels a task's commit trail as process history, post-hoc
// packaging, or a single closing checkpoint — or refuses to label it.
//
// The order of the checks is the design. Zero commits is answered first,
// because it needs no denominator. Then every way the denominator could fail
// to measure ONE agent run is exhausted, before any verdict is reached, so no
// refusal is ever silently resolved into a judgement. Only then does the
// commit count matter — and it is checked BEFORE the ratio, because a single
// commit has a zero-length window whose fraction is 0.0, which would otherwise
// read as the most extreme burst on record.
// Refs: MGIT-110, R-H234, FR-4, FR-16
func ClassifyCadence(obs CadenceObservation) CadenceEvidence {
	n := len(obs.CommitTimes)
	if n == 0 {
		return CadenceEvidence{Verdict: CadenceNoCommits, Summary: noCommitsSummary(obs)}
	}
	sorted := sortedTimes(obs.CommitTimes)
	if reason := denominatorRefusal(obs, sorted); reason != "" {
		return CadenceEvidence{
			Verdict: CadenceInsufficientEvidence, Reason: reason, Commits: n,
			Summary: refusalSummary(reason),
		}
	}

	first, last := sorted[0], sorted[n-1]
	elapsed := last.Sub(obs.RunStart)
	window := last.Sub(first)
	fraction := float64(window) / float64(elapsed)

	measure := &CadenceMeasure{
		Denominator:    CadenceDenominatorWorktree,
		RunStartAt:     rfc3339(obs.RunStart),
		FirstCommitAt:  rfc3339(first),
		LastCommitAt:   rfc3339(last),
		ElapsedSeconds: elapsed.Seconds(),
		WindowSeconds:  window.Seconds(),
		WindowFraction: fraction,
	}
	verdict := cadenceVerdictFor(n, fraction)
	return CadenceEvidence{
		Verdict: verdict, Commits: n, Measure: measure,
		Summary: verdictSummary(verdict, n, measure),
	}
}

// cadenceVerdictFor picks the verdict for a trail on a trusted denominator.
//
// The count is tested first and deliberately: a lone commit's window fraction
// is 0.0, so falling through to the ratio would report the commonest shape in
// this repo's history as the rarest one. Refs: MGIT-110
func cadenceVerdictFor(n int, fraction float64) string {
	switch {
	case n < 2:
		return CadenceSingleCheckpoint
	case fraction <= CadenceBurstFraction:
		return CadencePackagedPostHoc
	default:
		return CadenceSpreadAcrossRun
	}
}

// denominatorRefusal returns the reason the run itself cannot be measured, or
// "" when it can. It says nothing about how many commits there are: the count
// is an observation, not an obstacle. sorted must be ascending and non-empty.
// Refs: MGIT-110
func denominatorRefusal(obs CadenceObservation, sorted []time.Time) string {
	if !obs.HasRunStart {
		return CadenceReasonNoDenominator
	}
	if sorted[0].Before(obs.RunStart) {
		return CadenceReasonCommitsPrecedeWorktree
	}
	switch elapsed := sorted[len(sorted)-1].Sub(obs.RunStart); {
	case elapsed > CadenceMaxRun:
		return CadenceReasonRunTooLong
	case elapsed < CadenceMinRun:
		return CadenceReasonRunTooShort
	default:
		return ""
	}
}

// sortedTimes returns an ascending copy: the caller orders by `position`,
// which is not guaranteed to be chronological, and a negative window would
// read as a burst.
func sortedTimes(in []time.Time) []time.Time {
	out := make([]time.Time, len(in))
	copy(out, in)
	sort.Slice(out, func(i, j int) bool { return out[i].Before(out[j]) })
	return out
}

// rfc3339 renders one instant in the project's ISO-8601 UTC form.
func rfc3339(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// CadenceVerdicts returns the closed verdict set, in the order the golden
// test pins. Refs: MGIT-110
func CadenceVerdicts() []string {
	return []string{
		CadencePackagedPostHoc, CadenceSpreadAcrossRun, CadenceSingleCheckpoint,
		CadenceNoCommits, CadenceInsufficientEvidence,
	}
}

// CadenceReasons returns the closed refusal-reason set — every member a
// failure of the denominator, never of the commit count. Refs: MGIT-110
func CadenceReasons() []string {
	return []string{
		CadenceReasonNoDenominator, CadenceReasonCommitsPrecedeWorktree,
		CadenceReasonRunTooLong, CadenceReasonRunTooShort,
	}
}

// CadenceDenominators returns every denominator a verdict may rest on. One
// today; a second would be a reviewed addition, not an accident. Refs: MGIT-110
func CadenceDenominators() []string { return []string{CadenceDenominatorWorktree} }

// ValidCadenceVerdict reports membership of the closed verdict set.
// Refs: MGIT-110
func ValidCadenceVerdict(v string) bool { return contains(CadenceVerdicts(), v) }

// ValidCadenceReason reports membership of the closed refusal-reason set.
// Refs: MGIT-110
func ValidCadenceReason(r string) bool { return contains(CadenceReasons(), r) }

func contains(set []string, want string) bool {
	for _, s := range set {
		if s == want {
			return true
		}
	}
	return false
}

// noCommitsSummary states the zero-commit fact without dressing it up.
func noCommitsSummary(obs CadenceObservation) string {
	if !obs.HasRunStart {
		return "no commits recorded for this task."
	}
	return fmt.Sprintf(
		"no commits recorded for this task since its worktree was created at %s. "+
			"There is no checkpoint to return to.", rfc3339(obs.RunStart))
}

// refusalSummary explains what could not be measured, and why that is a
// refusal rather than a pass.
func refusalSummary(reason string) string {
	const lead = "cannot tell whether this trail is process history or post-hoc packaging: "
	switch reason {
	case CadenceReasonNoDenominator:
		return lead + "no worktree is registered for this task, so nothing records when " +
			"the run began. (`mgit work` registers one.)"
	case CadenceReasonCommitsPrecedeWorktree:
		return lead + "commits predate the worktree that holds them, so the recorded " +
			"start time is not measuring this work."
	case CadenceReasonRunTooLong:
		return lead + fmt.Sprintf(
			"the trail spans longer than one plausible agent run (over %s from worktree "+
				"creation), so the worktree covers more than one session.", CadenceMaxRun)
	case CadenceReasonRunTooShort:
		return lead + fmt.Sprintf(
			"the run is under %s, and on a run that short a tight commit window is "+
				"indistinguishable from complete coverage of it.", CadenceMinRun)
	default:
		return lead + "the reason is not one this build can explain."
	}
}

// verdictSummary states the observation in a reviewer's terms. Prose only —
// never match on it. Refs: MGIT-109
func verdictSummary(verdict string, n int, m *CadenceMeasure) string {
	if verdict == CadenceSingleCheckpoint {
		// Stated as an observation about the RUN, not a judgement about the
		// agent: the proportion is the whole content of this verdict, and the
		// window ("0s of it") would be noise.
		return fmt.Sprintf(
			"1 commit recorded across a %s run, and nothing before it. There is no earlier "+
				"point in this run to return to: the record holds a result, not the steps "+
				"to it.", roughDuration(m.ElapsedSeconds))
	}
	shape := fmt.Sprintf("%d commits written over %s of a %s run (%.0f%% of it)",
		n, roughDuration(m.WindowSeconds), roughDuration(m.ElapsedSeconds),
		m.WindowFraction*100)
	if verdict == CadencePackagedPostHoc {
		return shape + fmt.Sprintf(
			". This trail was packaged post-hoc; it is not process history. Read it as one "+
				"step recorded in %d parts, not as %d steps.", n, n)
	}
	return shape + ". The commits are spread across the run, which is consistent with " +
		"process history — it does not prove it."
}

// roughDuration renders a span at minute resolution, which is the resolution
// the underlying created_at timestamps actually carry.
func roughDuration(seconds float64) string {
	return time.Duration(seconds * float64(time.Second)).Round(time.Second).String()
}
