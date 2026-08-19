package branchguard_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/branchguard"
)

// fixture builds real go-git repositories with the branch shapes the guard has
// to judge. Every test below is an ancestry question, so the repositories are
// real rather than mocked — a fake ancestry graph would only test the fake.
type fixture struct {
	t    *testing.T
	dir  string
	repo *gogit.Repository
	wt   *gogit.Worktree
	at   time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	repo, err := gogit.PlainInitWithOptions(dir, &gogit.PlainInitOptions{
		InitOptions: gogit.InitOptions{DefaultBranch: plumbing.Main},
	})
	require.NoError(t, err)
	wt, err := repo.Worktree()
	require.NoError(t, err)
	return &fixture{t: t, dir: dir, repo: repo, wt: wt, at: time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)}
}

// commit writes each file and commits them, returning the new hash.
func (f *fixture) commit(msg string, files ...string) plumbing.Hash {
	f.t.Helper()
	for _, name := range files {
		abs := filepath.Join(f.dir, filepath.FromSlash(name))
		require.NoError(f.t, os.MkdirAll(filepath.Dir(abs), 0o750))
		require.NoError(f.t, os.WriteFile(abs, []byte(msg+"\n"+name+"\n"), 0o600))
		_, err := f.wt.Add(name)
		require.NoError(f.t, err)
	}
	f.at = f.at.Add(time.Minute)
	h, err := f.wt.Commit(msg, &gogit.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com", When: f.at},
	})
	require.NoError(f.t, err)
	return h
}

// checkoutNew is the `git checkout -b` the incident turned on: it branches from
// wherever HEAD currently stands, whatever that is.
func (f *fixture) checkoutNew(name string) {
	f.t.Helper()
	require.NoError(f.t, f.wt.Checkout(&gogit.CheckoutOptions{
		Branch: plumbing.NewBranchReferenceName(name), Create: true,
	}))
}

func (f *fixture) checkout(name string) {
	f.t.Helper()
	require.NoError(f.t, f.wt.Checkout(&gogit.CheckoutOptions{
		Branch: plumbing.NewBranchReferenceName(name),
	}))
}

// remoteMirror publishes a local branch as refs/remotes/origin/<name> without a
// network: the guard reads remote-tracking refs, and a pushed-but-unmerged
// branch is exactly what the incident inherited from.
func (f *fixture) remoteMirror(name string) {
	f.t.Helper()
	local, err := f.repo.Reference(plumbing.NewBranchReferenceName(name), true)
	require.NoError(f.t, err)
	rn := plumbing.NewRemoteReferenceName("origin", name)
	require.NoError(f.t, f.repo.Storer.SetReference(plumbing.NewHashReference(rn, local.Hash())))
	_, err = f.repo.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{"https://example.invalid/x.git"}})
	if err != nil && !errors.Is(err, gogit.ErrRemoteExists) {
		require.NoError(f.t, err)
	}
}

// mergeIntoMain fast-forwards main to a branch tip, standing in for a merged PR.
func (f *fixture) mergeIntoMain(name string) {
	f.t.Helper()
	tip, err := f.repo.Reference(plumbing.NewBranchReferenceName(name), true)
	require.NoError(f.t, err)
	require.NoError(f.t, f.repo.Storer.SetReference(
		plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), tip.Hash())))
}

func (f *fixture) check(t *testing.T, opts branchguard.Options) *branchguard.Result {
	t.Helper()
	res, err := branchguard.Check(f.repo, opts)
	require.NoError(t, err)
	return res
}

// incidentRepo reconstructs 9abf4ce: main, an unmerged task branch carrying the
// MGIT-118 classifier work, and a one-file CI branch cut with `git checkout -b`
// while standing on that task branch. Refs: MGIT-142, MGIT-118
func incidentRepo(t *testing.T) *fixture {
	f := newFixture(t)
	f.commit("chore: seed", "README.md")
	f.checkoutNew("fix/mgit-118-humble-default")
	f.commit("fix(cli): a default is not a diagnosis",
		"cmd/mgit/sandbox_guest_failure.go",
		"cmd/mgit/sandbox_entitlement.go",
		"cmd/mgit/sandbox_guest_failure_test.go",
		"docs/adr/014-guest-memory-exhaustion-is-not-detectable.md",
		"scripts/e2e/sandbox_fleet_soak.sh",
		"CHANGELOG.md")
	f.remoteMirror("fix/mgit-118-humble-default")
	// The defect: standing on the task branch, not on main.
	f.checkoutNew("fix/ci-kernel-tarball-retry")
	f.commit("ci: retry the libkrunfw build", "scripts/sandbox-image/build-libkrun.sh")
	return f
}

func TestCheck_BranchCutFromAnotherTaskBranch_ListsInheritedFiles(t *testing.T) {
	f := incidentRepo(t)

	res := f.check(t, branchguard.Options{Branch: "fix/ci-kernel-tarball-retry"})

	require.False(t, res.Clean(), "the 9abf4ce shape must not pass")
	require.Len(t, res.Inherited, 1)
	require.Contains(t, res.Inherited[0].Refs, "fix/mgit-118-humble-default")
	require.Len(t, res.Inherited[0].Commits, 1)
	require.Equal(t, "fix(cli): a default is not a diagnosis", res.Inherited[0].Commits[0].Subject)
	require.ElementsMatch(t, []string{
		"CHANGELOG.md",
		"cmd/mgit/sandbox_entitlement.go",
		"cmd/mgit/sandbox_guest_failure.go",
		"cmd/mgit/sandbox_guest_failure_test.go",
		"docs/adr/014-guest-memory-exhaustion-is-not-detectable.md",
		"scripts/e2e/sandbox_fleet_soak.sh",
	}, res.Files)
	require.NotContains(t, res.Files, "scripts/sandbox-image/build-libkrun.sh",
		"the branch's own file is in scope and must not be named")
}

func TestCheck_BranchCutFromMain_MultiFileWork_Clean(t *testing.T) {
	f := newFixture(t)
	f.commit("chore: seed", "README.md")
	f.checkoutNew("fix/mgit-118-humble-default")
	f.commit("fix(cli): unrelated task", "cmd/mgit/sandbox_guest_failure.go")
	f.remoteMirror("fix/mgit-118-humble-default")
	f.checkout("main")
	f.checkoutNew("feat/wide-but-legitimate")
	f.commit("feat: ten files, one task",
		"a.go", "b.go", "c.go", "d.go", "e.go", "f.go", "g.go", "h.go", "i.go", "j.go")
	f.commit("test: and their tests", "a_test.go", "b_test.go")

	res := f.check(t, branchguard.Options{Branch: "feat/wide-but-legitimate"})

	require.True(t, res.Clean(), "ordinary wide work cut from main must pass without ceremony")
	require.Empty(t, res.Files)
}

func TestCheck_ParentBranchMergedIntoMain_Clean(t *testing.T) {
	f := newFixture(t)
	f.commit("chore: seed", "README.md")
	f.checkoutNew("feat/landed")
	f.commit("feat: landed work", "landed.go")
	f.remoteMirror("feat/landed")
	f.mergeIntoMain("feat/landed")
	f.checkoutNew("fix/next")
	f.commit("fix: my own work", "next.go")

	res := f.check(t, branchguard.Options{Branch: "fix/next"})

	require.True(t, res.Clean(), "a parent whose commits are in main carries nothing extra into the PR")
}

func TestCheck_LocalBaseStaleBehindRemote_Clean(t *testing.T) {
	f := newFixture(t)
	f.commit("chore: seed", "README.md")
	// origin/main has advanced; the local main ref stays where it was.
	f.checkoutNew("tmp-upstream")
	f.commit("feat: upstream work", "upstream.go")
	f.remoteMirror("tmp-upstream")
	tip, err := f.repo.Reference(plumbing.NewBranchReferenceName("tmp-upstream"), true)
	require.NoError(t, err)
	require.NoError(t, f.repo.Storer.SetReference(plumbing.NewHashReference(
		plumbing.NewRemoteReferenceName("origin", "main"), tip.Hash())))
	f.checkoutNew("feat/from-origin-main")
	f.commit("feat: my work", "mine.go")

	res := f.check(t, branchguard.Options{Branch: "feat/from-origin-main"})

	require.True(t, res.Clean(), "cutting from origin/main while local main is stale is not an inheritance")
}

func TestCheck_DeclaredParentBase_Clean(t *testing.T) {
	f := incidentRepo(t)

	res := f.check(t, branchguard.Options{
		Branch: "fix/ci-kernel-tarball-retry",
		Bases:  []string{"fix/mgit-118-humble-default"},
	})

	require.True(t, res.Clean(), "an explicitly declared parent is a legitimate stack")
}

func TestCheck_OverrideTrailer_RecordsReasonAndStillNamesFiles(t *testing.T) {
	f := incidentRepo(t)
	f.commit("chore: stack on MGIT-118 deliberately\n\nBranch-Scope-Override: needs the 118 classifier to test the retry",
		"scripts/sandbox-image/build-libkrun.sh")

	res := f.check(t, branchguard.Options{Branch: "fix/ci-kernel-tarball-retry"})

	require.False(t, res.Clean(), "the override does not make the inheritance disappear")
	require.True(t, res.Overridden())
	require.Equal(t, "needs the 118 classifier to test the retry", res.Override.Reason)
	require.NotEmpty(t, res.Override.Commit.Hash)
	require.NotEmpty(t, res.Files)
}

func TestCheck_OverrideTrailerBlank_NotAnOverride(t *testing.T) {
	f := incidentRepo(t)
	f.commit("chore: try to slip past\n\nBranch-Scope-Override:   ", "scripts/sandbox-image/build-libkrun.sh")

	res := f.check(t, branchguard.Options{Branch: "fix/ci-kernel-tarball-retry"})

	require.False(t, res.Overridden(), "an empty reason records nothing and so overrides nothing")
}

func TestCheck_CutFromMiddleOfAnotherBranch_Detected(t *testing.T) {
	f := newFixture(t)
	f.commit("chore: seed", "README.md")
	f.checkoutNew("feat/other-task")
	f.commit("feat: other task part one", "other1.go")
	f.checkoutNew("fix/mine") // cut from the middle
	f.checkout("feat/other-task")
	f.commit("feat: other task part two", "other2.go")
	f.remoteMirror("feat/other-task")
	f.checkout("fix/mine")
	f.commit("fix: my own", "mine.go")

	res := f.check(t, branchguard.Options{Branch: "fix/mine"})

	require.False(t, res.Clean(), "sharing part of another branch is still inheriting it")
	require.Equal(t, []string{"other1.go"}, res.Files)
	require.NotContains(t, res.Files, "other2.go", "only the shared commits are inherited")
}

func TestCheck_BranchIsBase_Clean(t *testing.T) {
	f := newFixture(t)
	f.commit("chore: seed", "README.md")
	f.commit("chore: more", "second.go")

	res := f.check(t, branchguard.Options{Branch: "main"})

	require.True(t, res.Clean(), "main carries nothing main does not have")
}

func TestCheck_RenamedBranchSameTip_Clean(t *testing.T) {
	f := newFixture(t)
	f.commit("chore: seed", "README.md")
	f.checkoutNew("feat/work")
	f.commit("feat: work", "work.go")
	f.remoteMirror("feat/work")
	// A second name for the very same commits — a rename or a backup ref, not
	// an inheritance: there is no work of "our own" it could be out of scope of.
	f.checkoutNew("feat/work-v2")

	res := f.check(t, branchguard.Options{Branch: "feat/work-v2"})

	require.True(t, res.Clean(), "a ref pointing at exactly our own commits is not a parent")
}

func TestCheck_HeadDefaultsToCurrentBranch(t *testing.T) {
	f := incidentRepo(t)

	res := f.check(t, branchguard.Options{})

	require.Equal(t, "fix/ci-kernel-tarball-retry", res.Branch)
	require.False(t, res.Clean())
}

func TestCheck_UnknownBranch_ReturnsError(t *testing.T) {
	f := newFixture(t)
	f.commit("chore: seed", "README.md")

	_, err := branchguard.Check(f.repo, branchguard.Options{Branch: "no/such/branch"})

	require.Error(t, err)
	require.Contains(t, err.Error(), "no/such/branch")
}

func TestCheck_UnknownBase_ReturnsError(t *testing.T) {
	f := newFixture(t)
	f.commit("chore: seed", "README.md")

	_, err := branchguard.Check(f.repo, branchguard.Options{Bases: []string{"no/such/base"}})

	require.Error(t, err)
	require.Contains(t, err.Error(), "no/such/base")
}

func TestCheck_EmptyRepository_ReturnsError(t *testing.T) {
	f := newFixture(t)

	_, err := branchguard.Check(f.repo, branchguard.Options{})

	require.Error(t, err)
}

func TestSurvey_RealBranchShapes_CountsOnlyTheIncident(t *testing.T) {
	f := incidentRepo(t)
	f.checkout("main")
	f.checkoutNew("feat/legit-one")
	f.commit("feat: one", "one.go")
	f.remoteMirror("feat/legit-one")
	f.checkout("main")
	f.checkoutNew("feat/legit-two")
	f.commit("feat: two", "two.go", "two_test.go")
	f.remoteMirror("feat/legit-two")

	results, err := branchguard.Survey(f.repo, branchguard.Options{})
	require.NoError(t, err)

	var fired []string
	for _, r := range results {
		if !r.Clean() {
			fired = append(fired, r.Branch)
		}
	}
	require.Equal(t, []string{"fix/ci-kernel-tarball-retry"}, fired,
		"exactly the incident branch fires; the legitimate ones do not")
	require.GreaterOrEqual(t, len(results), 4, "survey covers every branch, not just the failing one")
}

func TestRefusal_NamesOutOfScopeFilesParentAndOverride(t *testing.T) {
	f := incidentRepo(t)
	res := f.check(t, branchguard.Options{Branch: "fix/ci-kernel-tarball-retry"})

	text := branchguard.Refusal(res)

	require.Contains(t, text, "fix/ci-kernel-tarball-retry")
	require.Contains(t, text, "fix/mgit-118-humble-default")
	require.Contains(t, text, "cmd/mgit/sandbox_guest_failure.go")
	require.Contains(t, text, "6 files")
	require.Contains(t, text, "git rebase --onto main fix/mgit-118-humble-default fix/ci-kernel-tarball-retry")
	require.Contains(t, text, branchguard.OverrideTrailer,
		"the override must be discoverable from the refusal itself")
	require.Contains(t, text, "--base fix/mgit-118-humble-default")
}

func TestRefusal_ManyFiles_TruncatesList(t *testing.T) {
	f := newFixture(t)
	f.commit("chore: seed", "README.md")
	f.checkoutNew("feat/other")
	names := make([]string, 0, 40)
	for i := range 40 {
		names = append(names, string(rune('a'+i%26))+string(rune('a'+i/26))+".go")
	}
	f.commit("feat: many", names...)
	f.remoteMirror("feat/other")
	f.checkoutNew("fix/mine")
	f.commit("fix: mine", "mine.go")

	text := branchguard.Refusal(f.check(t, branchguard.Options{Branch: "fix/mine"}))

	require.Contains(t, text, "40 files")
	require.Contains(t, text, "more")
	require.Less(t, strings.Count(text, ".go"), 40, "a refusal nobody can read is a refusal nobody acts on")
}

func TestOverrideNotice_NamesReasonAndCommit(t *testing.T) {
	f := incidentRepo(t)
	f.commit("chore: deliberate stack\n\nBranch-Scope-Override: rebuilding on 118 on purpose",
		"scripts/sandbox-image/build-libkrun.sh")
	res := f.check(t, branchguard.Options{Branch: "fix/ci-kernel-tarball-retry"})

	notice := branchguard.OverrideNotice(res)

	require.Contains(t, notice, "rebuilding on 118 on purpose")
	require.Contains(t, notice, res.Override.Commit.Hash[:7])
	require.Contains(t, notice, "fix/mgit-118-humble-default")
	require.Contains(t, notice, "6 files")
}

func TestCheck_TwoStackedParents_ReportsEachSeparately(t *testing.T) {
	f := newFixture(t)
	f.commit("chore: seed", "README.md")
	f.checkoutNew("feat/parent-one")
	f.commit("feat: parent one", "one.go")
	f.remoteMirror("feat/parent-one")
	f.checkoutNew("feat/parent-two")
	f.commit("feat: parent two", "two.go")
	f.remoteMirror("feat/parent-two")
	f.checkoutNew("fix/mine")
	f.commit("fix: mine", "mine.go")

	res := f.check(t, branchguard.Options{Branch: "fix/mine"})

	require.Len(t, res.Inherited, 2, "each ancestor branch is its own inheritance")
	require.ElementsMatch(t, []string{"one.go", "two.go"}, res.Files)
	text := branchguard.Refusal(res)
	require.Contains(t, text, "feat/parent-one")
	require.Contains(t, text, "feat/parent-two")
}

func TestCheck_ParentWithLocalAndRemoteTwin_ReportedOnce(t *testing.T) {
	f := incidentRepo(t)

	res := f.check(t, branchguard.Options{Branch: "fix/ci-kernel-tarball-retry"})

	require.Len(t, res.Inherited, 1)
	require.ElementsMatch(t,
		[]string{"fix/mgit-118-humble-default", "origin/fix/mgit-118-humble-default"},
		res.Inherited[0].Refs, "one branch under two names is one inheritance")
	require.Contains(t, branchguard.Refusal(res), "also origin/fix/mgit-118-humble-default")
}

func TestSurvey_UnknownBase_ReturnsError(t *testing.T) {
	f := newFixture(t)
	f.commit("chore: seed", "README.md")

	_, err := branchguard.Survey(f.repo, branchguard.Options{Bases: []string{"no/such/base"}})

	require.Error(t, err)
}
