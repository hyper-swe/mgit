package branchguard_test

import (
	"testing"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/branchguard"
)

// A REF NAMING THE BRANCH'S OWN PUSHED PAST IS NOT A PARENT (MGIT-254).
// Review tooling may store pull-request heads as local branches (`git fetch
// origin pull/N/head:pr-N`). Such a ref points at a commit the branch has
// ALREADY pushed, so after the author's next commit it shares a strict subset
// of the branch's commits, which is the parent shape. Those commits are the
// author's own, already on origin/<branch>. The measured case was #189's
// branch, refused for its own two commits "From: pr-189".

// pushedBranchWithReviewRef builds branch B with c1 and c2 on origin/B, a local
// ref pr-7 at c2 (the review tooling's copy of the pushed head), and the
// author's new commit c3 on top.
func pushedBranchWithReviewRef(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t)
	f.commit("chore: seed", "README.md")
	f.checkoutNew("fix/b")
	f.commit("fix: first step", "a.go")
	c2 := f.commit("fix: second step", "b.go")
	f.remoteMirror("fix/b")
	require.NoError(t, f.repo.Storer.SetReference(
		plumbing.NewHashReference(plumbing.NewBranchReferenceName("pr-7"), c2)))
	f.commit("fix: third step", "c.go")
	return f
}

func TestCheck_ReviewRefAtBranchsPushedHead_Clean(t *testing.T) {
	f := pushedBranchWithReviewRef(t)

	res := f.check(t, branchguard.Options{Branch: "fix/b"})

	require.True(t, res.Clean(), "pr-7 names commits already on origin/fix/b, so they are the branch's own; got %+v", res.Inherited)
}

// The exemption covers only commits already on the branch's own remote. A
// foreign branch's commit brought in after the push is still inheritance, even
// with a review ref present.
func TestCheck_ForeignParentAfterPush_StillRefused(t *testing.T) {
	f := pushedBranchWithReviewRef(t)
	f.checkout("main")
	f.checkoutNew("fix/other")
	foreign := f.commit("feat: someone else's work", "other.go")
	f.remoteMirror("fix/other")
	f.checkout("fix/b")
	tip, err := f.repo.Reference(plumbing.NewBranchReferenceName("fix/b"), true)
	require.NoError(t, err)
	f.at = f.at.Add(1)
	_, err = f.wt.Commit("merge: fold in the other branch", &gogit.CommitOptions{
		Author:            &object.Signature{Name: "Test", Email: "test@example.com", When: f.at},
		Parents:           []plumbing.Hash{tip.Hash(), foreign},
		AllowEmptyCommits: true,
	})
	require.NoError(t, err)

	res := f.check(t, branchguard.Options{Branch: "fix/b"})

	require.False(t, res.Clean(), "a commit not on origin/fix/b that another unmerged branch holds is inherited")
	require.Len(t, res.Inherited, 1)
	require.Contains(t, res.Inherited[0].Refs, "fix/other")
	require.Len(t, res.Inherited[0].Commits, 1)
	require.Equal(t, "feat: someone else's work", res.Inherited[0].Commits[0].Subject)
}
