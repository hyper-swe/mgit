package branchguard

import (
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// OverrideTrailer is the commit trailer that waives this guard for a branch.
//
// It is a TRAILER rather than a command-line flag on purpose, and that is the
// whole of the "recorded" requirement. A flag is typed once into a shell that
// nobody reads afterwards; a trailer is in the branch's own history, so it is
// in the pull request the reviewer opens, next to the wide diff it explains.
// The same property makes it work in CI, which has no shell to inherit a flag
// from. Refs: MGIT-142, MGIT-131
const OverrideTrailer = "Branch-Scope-Override:"

// findOverride returns the waiver recorded on the branch's OWN commits.
//
// Commits inherited from the other branch are excluded: a trailer written by
// somebody else, for another branch, is not this author's declaration of
// intent. The newest qualifying commit wins, so amending a fresh reason
// replaces a stale one. A blank reason is not an override — the point of the
// mechanism is the reason. Refs: MGIT-142
func findOverride(own map[plumbing.Hash]*object.Commit, inherited []Inherited) Override {
	skip := inheritedHashes(inherited)
	candidates := make(map[plumbing.Hash]*object.Commit, len(own))
	for h, c := range own {
		if _, ok := skip[h]; !ok {
			candidates[h] = c
		}
	}
	ordered := sortedCommits(candidates)
	for i := len(ordered) - 1; i >= 0; i-- {
		c := candidates[plumbing.NewHash(ordered[i].Hash)]
		if reason := overrideReason(c.Message); reason != "" {
			return Override{Reason: reason, Commit: ordered[i]}
		}
	}
	return Override{}
}

// overrideReason extracts the trailer's reason from a commit message, or "".
// Refs: MGIT-142
func overrideReason(message string) string {
	for _, line := range strings.Split(message, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), OverrideTrailer)
		if !ok {
			continue
		}
		if reason := strings.TrimSpace(rest); reason != "" {
			return reason
		}
	}
	return ""
}

// inheritedHashes indexes the commits that came from another branch.
// Refs: MGIT-142
func inheritedHashes(inherited []Inherited) map[plumbing.Hash]struct{} {
	set := map[plumbing.Hash]struct{}{}
	for _, in := range inherited {
		for _, c := range in.Commits {
			set[plumbing.NewHash(c.Hash)] = struct{}{}
		}
	}
	return set
}
