// Package branchguard refuses a branch that carries another branch's commits,
// before the branch becomes a pull request.
//
// THE INCIDENT IT EXISTS FOR. `git checkout -b` silently inherits whatever
// branch you are standing on. On 2026-08-19 a branch cut for a ONE-FILE CI
// change was cut while standing on an unmerged task branch, so it inherited
// that task's entire classifier rewrite: 9abf4ce landed on main describing a
// 24-line change to one shell script while actually carrying 531 lines across
// six files. Every gate was green and the PR diff showed all six files; what
// failed was that nothing MECHANICALLY compared the branch's contents against
// where the branch was supposed to have started.
//
// WHAT "SCOPE" MEANS HERE, and why it is the cheap definition. A branch's scope
// is "the commits its author wrote on it". Anything else in `base..HEAD` was
// inherited from somewhere, and inherited commits are the entire mechanism of
// the incident. So the rule needs no ticket metadata, no declared file list and
// no per-task plumbing:
//
//	a branch may not carry commits that belong to another unmerged branch.
//
// Detection is ancestry, not heuristics: a commit in `base..HEAD` that is also
// reachable from some other unmerged ref was not written on this branch. That
// fires on the incident (the MGIT-118 branch's commit is reachable from the CI
// branch) and cannot fire on a branch cut from main, whose commits are
// reachable from nothing else. A richer definition — files named in the ticket,
// a scope declared at `mgit work` time — would need metadata this project's
// tickets do not carry, and would have caught exactly the same one branch.
//
// Refs: MGIT-142, MGIT-131, MGIT-118, R-H285, R-H286
package branchguard

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// DefaultBases are the refs a branch is assumed to have been cut from when the
// caller declares nothing. Both are consulted because a developer who cuts from
// origin/main while their local main is stale has done nothing wrong, and a
// guard that calls that an inheritance is noise. Refs: MGIT-142
var DefaultBases = []string{"main", "origin/main"}

// ErrNoBase reports that not one of the candidate base refs exists in the
// repository, so there is nothing to measure the branch against.
var ErrNoBase = errors.New("no base ref found")

// Options selects what Check judges: which branch, and what it was cut from.
// Refs: MGIT-142
type Options struct {
	// Branch is the ref to check. Empty means HEAD's current branch.
	Branch string
	// Bases are the refs the branch is expected to have been cut from. Empty
	// means DefaultBases. Naming a base DECLARES a deliberate stack: commits
	// reachable from it are in scope by definition.
	Bases []string
}

// Commit is the identity of one commit as a refusal needs to print it.
// Refs: MGIT-142
type Commit struct {
	Hash    string `json:"hash"`
	Subject string `json:"subject"`
}

// Inherited is one other branch's contribution to the branch under check. Refs
// holds every name pointing at the same commits — a local branch and its
// remote-tracking twin are one inheritance, not two. Refs: MGIT-142
type Inherited struct {
	Refs    []string `json:"refs"`
	Commits []Commit `json:"commits"`
	Files   []string `json:"files"`
}

// Override is a recorded waiver: the reason from a Branch-Scope-Override
// trailer and the commit that carries it. The trailer is chosen over a
// command-line flag deliberately — a flag is typed once and vanishes, while a
// trailer travels in the history the reviewer reads on the PR. Refs: MGIT-142
type Override struct {
	Reason string `json:"reason"`
	Commit Commit `json:"commit"`
}

// Result is one branch's verdict. Refs: MGIT-142
type Result struct {
	Branch    string      `json:"branch"`
	Bases     []string    `json:"bases"`
	Inherited []Inherited `json:"inherited,omitempty"`
	Files     []string    `json:"files,omitempty"`
	Override  Override    `json:"override,omitempty"`
}

// Clean reports whether the branch carries only its own commits.
// Refs: MGIT-142
func (r *Result) Clean() bool { return len(r.Inherited) == 0 }

// Overridden reports whether a recorded waiver applies to this branch.
// Refs: MGIT-142
func (r *Result) Overridden() bool { return strings.TrimSpace(r.Override.Reason) != "" }

// Check judges one branch: it resolves the base, collects the commits the
// branch adds to it, and asks which of those commits some OTHER unmerged ref
// also holds. Refs: MGIT-142
func Check(repo *gogit.Repository, opts Options) (*Result, error) {
	branch, tip, err := resolveBranch(repo, opts.Branch)
	if err != nil {
		return nil, err
	}
	bases, baseHashes, err := resolveBases(repo, opts.Bases)
	if err != nil {
		return nil, err
	}
	baseSet, err := reachable(repo, baseHashes, nil)
	if err != nil {
		return nil, fmt.Errorf("walk base refs: %w", err)
	}
	own, err := reachable(repo, []plumbing.Hash{tip}, baseSet)
	if err != nil {
		return nil, fmt.Errorf("walk %s: %w", branch, err)
	}
	res := &Result{Branch: branch, Bases: bases}
	if len(own) == 0 {
		return res, nil
	}
	if res.Inherited, err = inheritedFrom(repo, branch, bases, baseSet, own); err != nil {
		return nil, err
	}
	res.Files = unionFiles(res.Inherited)
	res.Override = findOverride(own, res.Inherited)
	return res, nil
}

// resolveBranch turns Options.Branch into a name and a tip, defaulting to the
// checked-out branch so the pre-push hook and a bare manual run agree.
// Refs: MGIT-142
func resolveBranch(repo *gogit.Repository, name string) (string, plumbing.Hash, error) {
	if name == "" {
		head, err := repo.Head()
		if err != nil {
			return "", plumbing.ZeroHash, fmt.Errorf("resolve HEAD: %w", err)
		}
		return shortRefName(head.Name().String()), head.Hash(), nil
	}
	h, err := repo.ResolveRevision(plumbing.Revision(name))
	if err != nil {
		return "", plumbing.ZeroHash, fmt.Errorf("resolve branch %q: %w", name, err)
	}
	return shortRefName(name), *h, nil
}

// resolveBases resolves the declared bases (all of which must exist — a typo
// must not silently widen the branch's scope) or the defaults (of which the
// ones that exist are used, since not every clone has an origin).
// Refs: MGIT-142
func resolveBases(repo *gogit.Repository, declared []string) ([]string, []plumbing.Hash, error) {
	names, hashes := []string{}, []plumbing.Hash{}
	for _, name := range orDefault(declared) {
		h, err := repo.ResolveRevision(plumbing.Revision(name))
		if err != nil {
			if len(declared) > 0 {
				return nil, nil, fmt.Errorf("resolve base %q: %w", name, err)
			}
			continue
		}
		names, hashes = append(names, name), append(hashes, *h)
	}
	if len(hashes) == 0 {
		return nil, nil, fmt.Errorf("%w: tried %s", ErrNoBase, strings.Join(orDefault(declared), ", "))
	}
	return names, hashes, nil
}

func orDefault(declared []string) []string {
	if len(declared) == 0 {
		return DefaultBases
	}
	return declared
}

// reachable walks every ancestor of roots, stopping at anything in stop, and
// returns what it found. The commits are held rather than counted because the
// refusal has to name them and diff them. Refs: MGIT-142
func reachable(repo *gogit.Repository, roots []plumbing.Hash, stop map[plumbing.Hash]*object.Commit) (map[plumbing.Hash]*object.Commit, error) {
	seen := make(map[plumbing.Hash]*object.Commit, len(roots))
	queue := append([]plumbing.Hash(nil), roots...)
	for len(queue) > 0 {
		h := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if _, done := seen[h]; done {
			continue
		}
		if _, skip := stop[h]; skip {
			continue
		}
		c, err := object.GetCommit(repo.Storer, h)
		if err != nil {
			return nil, fmt.Errorf("read commit %s: %w", h.String(), err)
		}
		seen[h] = c
		queue = append(queue, c.ParentHashes...)
	}
	return seen, nil
}

// sortedCommits renders a commit set in the order a reader expects: oldest
// first, ties broken by hash so the output is deterministic. Refs: MGIT-142
func sortedCommits(set map[plumbing.Hash]*object.Commit) []Commit {
	commits := make([]*object.Commit, 0, len(set))
	for _, c := range set {
		commits = append(commits, c)
	}
	sort.Slice(commits, func(i, j int) bool {
		if !commits[i].Committer.When.Equal(commits[j].Committer.When) {
			return commits[i].Committer.When.Before(commits[j].Committer.When)
		}
		return commits[i].Hash.String() < commits[j].Hash.String()
	})
	out := make([]Commit, 0, len(commits))
	for _, c := range commits {
		out = append(out, Commit{Hash: c.Hash.String(), Subject: subject(c.Message)})
	}
	return out
}

func subject(message string) string {
	if i := strings.IndexByte(message, '\n'); i >= 0 {
		return strings.TrimSpace(message[:i])
	}
	return strings.TrimSpace(message)
}
