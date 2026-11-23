package branchguard_test

import (
	"fmt"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/branchguard"
)

// chainedTrainRepo builds the shape that turned main red on 2026-11-23: a
// merge-train branch that merged one pull request onto main, a second train
// built on top of the first, and main taking the first pull request as its own
// commit, so the first train's commit never reaches main. A third train cut
// from main stands for every other train. Refs: MGIT-343
func chainedTrainRepo(t *testing.T) *fixture {
	f := newFixture(t)
	f.commit("chore: seed", "README.md")
	f.checkoutNew("train/m381")
	f.commit("train: #381 on main", "a.go")
	f.remoteMirror("train/m381")
	f.checkoutNew("train/m385")
	f.commit("train: #385 on #381 train", "b.go")
	f.remoteMirror("train/m385")
	f.checkout("main")
	f.commit("merge #381", "a.go")
	f.checkoutNew("train/m390")
	f.commit("train: #390 on main", "c.go")
	f.remoteMirror("train/m390")
	f.checkout("main")
	// A CI clone holds the trains only as remote-tracking refs: no local
	// branch, so nothing is exempted as the branch's own pushed past.
	for _, b := range []string{"train/m381", "train/m385", "train/m390"} {
		require.NoError(t, f.repo.Storer.RemoveReference(plumbing.NewBranchReferenceName(b)))
	}
	return f
}

// The rule's answer on a chained train is pinned here, where it does not depend
// on which branches a clone happens to hold: the train built on another
// unmerged train carries that train's commit, so it is refused and names it;
// the train it was built on, and a train cut from main, are clean.
// Refs: MGIT-343, MGIT-142
func TestSurvey_ATrainBuiltOnAnotherUnmergedTrainIsRefusedAndNamesIt(t *testing.T) {
	f := chainedTrainRepo(t)

	results, err := branchguard.Survey(f.repo, branchguard.Options{})
	require.NoError(t, err)

	refused := map[string]*branchguard.Result{}
	for _, r := range results {
		if !r.Clean() {
			refused[r.Branch] = r
		}
	}
	require.Len(t, refused, 1, "only the chained train is refused: %v", keys(refused))
	res, ok := refused["train/m385"]
	require.True(t, ok, "the chained train is refused: %v", keys(refused))
	require.Contains(t, branchguard.Refusal(res), "train/m381")
	require.ElementsMatch(t, []string{"a.go"}, res.Files)
}

// failRecorder stands in for *testing.T so a test can see whether the live
// corpus judgement would fail, without failing itself.
type failRecorder struct {
	failed bool
	logs   []string
}

func (r *failRecorder) Errorf(format string, args ...any) {
	r.failed = true
	r.logs = append(r.logs, fmt.Sprintf(format, args...))
}
func (r *failRecorder) FailNow() { r.failed = true }
func (r *failRecorder) Helper()  {}
func (r *failRecorder) Logf(format string, args ...any) {
	r.logs = append(r.logs, fmt.Sprintf(format, args...))
}

// The live survey reads whatever branches the clone holds (CI fetches every
// remote branch), so a branch someone else pushed must not decide whether the
// suite passes. By default it reports a refusal it does not know; only a run
// that asks for the live corpus (MGIT_LIVE_BRANCH_SURVEY=1) fails on it.
// Refs: MGIT-343
func TestJudgeLiveCorpus_AnUnknownRefusalFailsOnlyWhenTheLiveSurveyIsAskedFor(t *testing.T) {
	f := chainedTrainRepo(t)
	results, err := branchguard.Survey(f.repo, branchguard.Options{})
	require.NoError(t, err)

	t.Setenv("MGIT_LIVE_BRANCH_SURVEY", "")
	quiet := &failRecorder{}
	judgeLiveCorpus(quiet, results)
	require.False(t, quiet.failed, "a refusal in the clone's live branches failed the default run")
	require.Contains(t, fmt.Sprint(quiet.logs), "train/m385", "the default run still reports the refusal")

	t.Setenv("MGIT_LIVE_BRANCH_SURVEY", "1")
	asked := &failRecorder{}
	judgeLiveCorpus(asked, results)
	require.True(t, asked.failed, "the live survey, when asked for, no longer fails on an unknown refusal")
}

func keys(m map[string]*branchguard.Result) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
