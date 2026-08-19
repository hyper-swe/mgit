package branchguard

import (
	"fmt"
	"sort"
	"strings"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// refCandidate is one branch ref the guard may accuse of being the parent.
type refCandidate struct {
	display string // "fix/x" for a local branch, "origin/fix/x" for a remote one
	remote  bool
	hash    plumbing.Hash
}

// inheritedFrom finds every other unmerged ref that shares commits with the
// branch under check, grouped so that a local branch and its remote-tracking
// twin are reported once. Refs: MGIT-142
func inheritedFrom(repo *gogit.Repository, branch string, bases []string,
	baseSet, own map[plumbing.Hash]*object.Commit) ([]Inherited, error) {
	excluded := identitySet(append([]string{branch}, bases...))
	refs, err := candidateRefs(repo, excluded)
	if err != nil {
		return nil, err
	}
	groups := map[string]*Inherited{}
	order := []string{}
	for _, ref := range refs {
		shared, err := sharedCommits(repo, ref.hash, baseSet, own)
		if err != nil {
			return nil, err
		}
		if !isParent(shared, own) {
			continue
		}
		sig := signature(shared)
		if g, ok := groups[sig]; ok {
			g.Refs = append(g.Refs, ref.display)
			continue
		}
		files, err := changedFiles(shared)
		if err != nil {
			return nil, err
		}
		groups[sig] = &Inherited{Refs: []string{ref.display}, Commits: sortedCommits(shared), Files: files}
		order = append(order, sig)
	}
	return collect(groups, order), nil
}

// isParent decides whether a ref's shared commits make it a PARENT of the
// branch rather than a child or an alias of it.
//
// Sharing NOTHING is the ordinary case: a branch cut from main shares no
// unmerged commit with anything. Sharing EVERYTHING is a second name for the
// same work — a rename, a backup ref, or a descendant branch looking back at
// the branch it was cut from — and there is no "out of scope" in that: every
// commit would be named, which is a refusal a reader cannot act on. What
// remains, a strict non-empty subset, is the incident: someone else's commits
// underneath our own. Refs: MGIT-142
func isParent(shared, own map[plumbing.Hash]*object.Commit) bool {
	return len(shared) > 0 && len(shared) < len(own)
}

// sharedCommits returns the commits a ref adds to the base that the branch
// under check also carries. Refs: MGIT-142
func sharedCommits(repo *gogit.Repository, tip plumbing.Hash,
	baseSet, own map[plumbing.Hash]*object.Commit) (map[plumbing.Hash]*object.Commit, error) {
	refOwn, err := reachable(repo, []plumbing.Hash{tip}, baseSet)
	if err != nil {
		return nil, fmt.Errorf("walk candidate ref %s: %w", tip.String(), err)
	}
	shared := make(map[plumbing.Hash]*object.Commit, len(refOwn))
	for h, c := range refOwn {
		if _, ok := own[h]; ok {
			shared[h] = c
		}
	}
	return shared, nil
}

// candidateRefs lists the local and remote-tracking branches that could be a
// parent: everything except the branch itself, the bases, and the symbolic
// pointers (origin/HEAD) that only alias another ref. Refs: MGIT-142
func candidateRefs(repo *gogit.Repository, excluded map[string]struct{}) ([]refCandidate, error) {
	iter, err := repo.References()
	if err != nil {
		return nil, fmt.Errorf("list refs: %w", err)
	}
	defer iter.Close()
	var out []refCandidate
	err = iter.ForEach(func(ref *plumbing.Reference) error {
		name := ref.Name().String()
		if ref.Type() != plumbing.HashReference || strings.HasSuffix(name, "/HEAD") {
			return nil
		}
		if !strings.HasPrefix(name, "refs/heads/") && !strings.HasPrefix(name, "refs/remotes/") {
			return nil
		}
		if _, skip := excluded[shortRefName(name)]; skip {
			return nil
		}
		out = append(out, refCandidate{display: displayRefName(name),
			remote: strings.HasPrefix(name, "refs/remotes/"), hash: ref.Hash()})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list refs: %w", err)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].remote != out[j].remote {
			return !out[i].remote // a local name is the one you can rebase onto
		}
		return out[i].display < out[j].display
	})
	return out, nil
}

// identitySet reduces ref names to the branch identity used for exclusion, so
// that "main", "refs/heads/main" and "origin/main" are one thing.
// Refs: MGIT-142
func identitySet(names []string) map[string]struct{} {
	set := make(map[string]struct{}, len(names)*2)
	for _, n := range names {
		set[shortRefName(n)] = struct{}{}
	}
	return set
}

// shortRefName strips refs/heads/, refs/remotes/<remote>/ and a bare
// <remote>/ prefix, leaving the branch identity. Refs: MGIT-142
func shortRefName(name string) string {
	name = strings.TrimPrefix(name, "refs/heads/")
	if rest, ok := strings.CutPrefix(name, "refs/remotes/"); ok {
		if _, after, found := strings.Cut(rest, "/"); found {
			return after
		}
		return rest
	}
	if before, after, found := strings.Cut(name, "/"); found && !strings.Contains(before, "refs") {
		// A bare "origin/main"-style revision; only strip a known remote-ish
		// first segment when what follows is itself a plausible branch name.
		if before == "origin" || before == "upstream" {
			return after
		}
	}
	return name
}

// displayRefName is what a human types back: the local branch name, or the
// remote-qualified name for a remote-tracking ref. Refs: MGIT-142
func displayRefName(name string) string {
	if rest, ok := strings.CutPrefix(name, "refs/remotes/"); ok {
		return rest
	}
	return strings.TrimPrefix(name, "refs/heads/")
}

// signature identifies a commit set so two refs holding the same commits are
// reported as one inheritance. Refs: MGIT-142
func signature(set map[plumbing.Hash]*object.Commit) string {
	hashes := make([]string, 0, len(set))
	for h := range set {
		hashes = append(hashes, h.String())
	}
	sort.Strings(hashes)
	return strings.Join(hashes, ",")
}

// collect flattens the grouped inheritances in discovery order.
// Refs: MGIT-142
func collect(groups map[string]*Inherited, order []string) []Inherited {
	out := make([]Inherited, 0, len(order))
	for _, sig := range order {
		out = append(out, *groups[sig])
	}
	return out
}

// unionFiles is every out-of-scope path across all inheritances, deduplicated
// and sorted — the list the refusal prints. Refs: MGIT-142
func unionFiles(inherited []Inherited) []string {
	seen := map[string]struct{}{}
	for _, in := range inherited {
		for _, f := range in.Files {
			seen[f] = struct{}{}
		}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for f := range seen {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}
