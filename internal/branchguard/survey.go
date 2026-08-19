package branchguard

import (
	"sort"

	gogit "github.com/go-git/go-git/v5"
)

// Survey runs Check over every branch in the repository, so the rule can be
// measured against real history rather than argued about.
//
// It exists because a tripwire that fires on legitimate work gets switched off
// within a week: the only way to know is to run the rule over the branches this
// project actually produced and count. A branch that exists both locally and as
// a remote-tracking ref is surveyed once, under its local name.
// Refs: MGIT-142, MGIT-131
func Survey(repo *gogit.Repository, opts Options) ([]*Result, error) {
	bases, _, err := resolveBases(repo, opts.Bases)
	if err != nil {
		return nil, err
	}
	refs, err := candidateRefs(repo, identitySet(bases))
	if err != nil {
		return nil, err
	}
	seen := map[string]struct{}{}
	var out []*Result
	for _, ref := range refs {
		id := shortRefName(ref.display)
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		res, err := Check(repo, Options{Branch: ref.display, Bases: opts.Bases})
		if err != nil {
			return nil, err
		}
		out = append(out, res)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Branch < out[j].Branch })
	return out, nil
}
