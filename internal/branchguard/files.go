package branchguard

import (
	"context"
	"fmt"
	"sort"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// diffOptions pins the tree diff to the cheap, deterministic form. Rename
// detection is off deliberately — the question is which paths a reviewer will
// see in the branch's diff, not how those paths got their names.
var diffOptions = &object.DiffTreeOptions{DetectRenames: false}

// changedFiles is every path the given commits touch, against their own first
// parent. Those paths ARE the out-of-scope set: they appear in the branch's
// diff, and in its pull request, without any commit of the author's having put
// them there. Refs: MGIT-142
func changedFiles(commits map[plumbing.Hash]*object.Commit) ([]string, error) {
	ctx := context.Background()
	paths := map[string]struct{}{}
	for _, c := range commits {
		if err := commitFiles(ctx, c, paths); err != nil {
			return nil, err
		}
	}
	return sortedPaths(paths), nil
}

// commitFiles is every path one commit changes relative to its first parent,
// or its whole tree when it is a root commit. Refs: MGIT-142
func commitFiles(ctx context.Context, c *object.Commit, into map[string]struct{}) error {
	tree, err := c.Tree()
	if err != nil {
		return fmt.Errorf("read tree of %s: %w", c.Hash.String(), err)
	}
	if c.NumParents() == 0 {
		return tree.Files().ForEach(func(f *object.File) error {
			into[f.Name] = struct{}{}
			return nil
		})
	}
	parent, err := c.Parent(0)
	if err != nil {
		return fmt.Errorf("read parent of %s: %w", c.Hash.String(), err)
	}
	parentTree, err := parent.Tree()
	if err != nil {
		return fmt.Errorf("read tree of %s: %w", parent.Hash.String(), err)
	}
	changes, err := object.DiffTreeWithOptions(ctx, parentTree, tree, diffOptions)
	if err != nil {
		return fmt.Errorf("diff %s: %w", c.Hash.String(), err)
	}
	for _, ch := range changes {
		if ch.From.Name != "" {
			into[ch.From.Name] = struct{}{}
		}
		if ch.To.Name != "" {
			into[ch.To.Name] = struct{}{}
		}
	}
	return nil
}

// sortedPaths renders a path set deterministically. Refs: MGIT-142
func sortedPaths(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
