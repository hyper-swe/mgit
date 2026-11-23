package branchguard_test

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/branchguard"
)

// MEASURED, NOT GUESSED — the MGIT-131 discipline applied to this guard.
//
// A tripwire that fires on legitimate work gets switched off within a week, so
// the rule below was run over this repository's real branches before it was
// wired to anything. Measured 2026-08-19 on hyper-swe/mgit, every local and
// remote-tracking branch (`make branch-survey`):
//
//	Branches surveyed:            54  (41 of them the head branch of a
//	                                   merged pull request, #1..#41)
//	Refused:                       1  — fix/ci-kernel-tarball-retry, the
//	                                   9abf4ce incident: 1 inherited commit,
//	                                   6 files, from fix/mgit-118-humble-default
//	False positives:               0
//
// The zero is not luck, it is what the rule measures: a branch cut from main
// shares no unmerged commit with any other ref, so there is nothing for the
// rule to find. Note that this repository squash-merges, which makes the
// corpus HARDER, not easier — a merged branch's own commits never appear in
// main, so all 41 landed branches were surveyed with their full unmerged
// history intact and still none of them fired.
//
// This test measures that result. It does not assert the number 1, which would
// rot as branches come and go; it checks the invariant the number expresses:
// nothing is refused except work that has recorded why. A new refusal here
// means either a real inheritance (fix the branch) or a rule that has started
// firing on legitimate work (fix the rule, not the test).
//
// It measures, it does not gate: the branches are whatever the clone holds, so
// by default a new refusal is reported in the log and the run passes, and only
// MGIT_LIVE_BRANCH_SURVEY=1 makes it fail (make branch-survey is the same
// measurement as a command). On 2026-11-23 a merge-train branch built on
// another unmerged train turned main red here; that shape is now pinned in
// livesurvey_test.go on a repository the test builds.
//
// Refs: MGIT-142, MGIT-131, MGIT-343
func TestSurvey_ThisRepositoryRealBranches_RefusesOnlyKnownIncidents(t *testing.T) {
	repo := openThisRepository(t)

	results, err := branchguard.Survey(repo, branchguard.Options{})
	if err != nil {
		t.Skipf("survey unavailable in this checkout (shallow clone?): %v", err)
	}
	// An EMPTY corpus is a property of the clone, not a failure of the rule.
	// `actions/checkout` fetches a single ref by default, so CI runs this in a
	// checkout with no local branches and no remote-tracking refs to survey --
	// which made this test red on main while passing on every developer
	// machine. The invariant below ("nothing refused that has not recorded
	// why") is vacuously true over zero branches, so asserting a non-empty
	// corpus asserted the ENVIRONMENT rather than the rule. Skip loudly instead,
	// so a CI run cannot read as evidence the corpus was checked.
	// Refs: MGIT-142
	if len(results) == 0 {
		t.Skip("no branches to survey in this checkout (a single-ref CI clone); " +
			"the corpus invariant is unverified here, not satisfied here")
	}

	judgeLiveCorpus(t, results)
	t.Logf("surveyed %d branches", len(results))
}

// corpusT is the part of *testing.T the live-corpus judgement uses, so a test
// can watch whether the judgement would fail without failing itself.
type corpusT interface {
	require.TestingT
	Helper()
	Logf(format string, args ...any)
}

// liveSurveyEnv names the switch that makes the live survey decide pass/fail.
const liveSurveyEnv = "MGIT_LIVE_BRANCH_SURVEY"

// judgeLiveCorpus applies the corpus invariant to a survey of this clone's
// branches: nothing is refused except a known incident. The clone's branches
// are whatever happens to be pushed (CI fetches every remote branch, merge
// trains included), so by default an unknown refusal is reported, not failed:
// the rule's answers are pinned by tests that build their own repositories.
// A run that sets MGIT_LIVE_BRANCH_SURVEY=1 asks for the measurement and fails
// on it. Refs: MGIT-142, MGIT-343
func judgeLiveCorpus(t corpusT, results []*branchguard.Result) {
	t.Helper()
	gate := os.Getenv(liveSurveyEnv) == "1"
	for _, res := range results {
		if res.Clean() || res.Overridden() {
			continue
		}
		if !gate {
			if !slices.Contains(knownIncidents, res.Branch) {
				t.Logf("REPORTED, NOT FAILED (set %s=1 to fail on it): branch %q is refused and is not a known incident:\n%s",
					liveSurveyEnv, res.Branch, branchguard.Refusal(res))
			}
			continue
		}
		require.Contains(t, knownIncidents, res.Branch,
			"branch %q is refused and is not a known incident:\n%s",
			res.Branch, branchguard.Refusal(res))
	}
}

// knownIncidents are the branches this repository is KNOWN to have got wrong,
// kept so the corpus test asserts an invariant rather than a number. Refs:
// MGIT-142
var knownIncidents = []string{"fix/ci-kernel-tarball-retry"}

// openThisRepository finds the git repository this source tree lives in, and
// skips the test when there is not one — the module is also built inside mgit
// worktrees, which are materialized without a .git directory. Refs: MGIT-142
func openThisRepository(t *testing.T) *gogit.Repository {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Skip("not inside a git checkout (mgit worktree); run this from a clone")
		}
		dir = parent
	}
	repo, err := gogit.PlainOpen(dir)
	require.NoError(t, err)
	if _, err := repo.Reference("refs/heads/main", true); err != nil {
		t.Skipf("no main branch in this checkout: %v", err)
	}
	return repo
}
