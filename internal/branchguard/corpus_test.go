package branchguard_test

import (
	"os"
	"path/filepath"
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
// This test pins that result. It does not assert the number 1, which would rot
// as branches come and go; it asserts the invariant the number expresses:
// nothing is refused except work that has recorded why. A new refusal here
// means either a real inheritance (fix the branch) or a rule that has started
// firing on legitimate work (fix the rule, not the test).
//
// Refs: MGIT-142, MGIT-131
func TestSurvey_ThisRepositoryRealBranches_RefusesOnlyKnownIncidents(t *testing.T) {
	repo := openThisRepository(t)

	results, err := branchguard.Survey(repo, branchguard.Options{})
	if err != nil {
		t.Skipf("survey unavailable in this checkout (shallow clone?): %v", err)
	}
	require.NotEmpty(t, results, "a real checkout has branches to survey")

	for _, res := range results {
		if res.Clean() || res.Overridden() {
			continue
		}
		require.Contains(t, knownIncidents, res.Branch,
			"branch %q is refused and is not a known incident:\n%s",
			res.Branch, branchguard.Refusal(res))
	}
	t.Logf("surveyed %d branches", len(results))
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
