package git

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/store/gitref"
)

// resyncPaths excludes ignored paths Git does not track, even when an earlier
// foundation capture put them in mgit's base. Ordinary task staging still
// honors mgit's own tracked set; this policy is only for base housekeeping.
// Without a readable Git commit, mgit remains the tracking authority.
// Refs: MGIT-290, ADR-008 §3
func (r *Repository) resyncPaths(paths []string) ([]string, error) {
	matcher, err := r.ignoreMatcher()
	if err != nil {
		return nil, err
	}
	var committed map[string]string
	loaded := false
	var out []string
	for _, p := range paths {
		if matcher.Match(strings.Split(p, "/"), false) {
			if !loaded {
				committed, err = gitref.CommittedBlobs(r.root)
				if errors.Is(err, gitref.ErrNoGit) || errors.Is(err, gitref.ErrDetachedOrUnborn) {
					return paths, nil
				}
				if err != nil {
					return nil, fmt.Errorf("resync: read Git tracked paths: %w", err)
				}
				loaded = true
			}
			if _, tracked := committed[p]; !tracked {
				continue
			}
		}
		out = append(out, p)
	}
	return out, nil
}

// WorkingTreeFingerprintForResync hashes the housekeeping working set. A new
// exclusion changes the signal even if .git/info/exclude is outside the walk.
// Refs: MGIT-290, ADR-008 §3
func (r *Repository) WorkingTreeFingerprintForResync() (string, error) {
	paths, err := r.listWorkingFiles()
	if err != nil {
		return "", err
	}
	paths, err = r.resyncPaths(paths)
	if err != nil {
		return "", err
	}
	return r.fingerprintPaths(paths)
}

// buildResyncTree removes obsolete ignored imports from the new tree, never
// from disk or earlier commits. Git-tracked ignored files keep their content
// and mode; unignored local foundation remains an explicit capture.
// Refs: MGIT-290, ADR-008 §3
func (cs *CommitStore) buildResyncTree() (plumbing.Hash, error) {
	hash, err := cs.buildTreeFromStaging()
	if err != nil {
		return plumbing.ZeroHash, err
	}
	tree, err := cs.repo.repo.TreeObject(hash)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	entries, err := flattenTree(tree)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	paths := make([]string, 0, len(entries))
	for p := range entries {
		paths = append(paths, p)
	}
	kept, err := cs.repo.resyncPaths(paths)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	selected := make(map[string]blobEntry, len(kept))
	for _, p := range kept {
		selected[p] = entries[p]
	}
	return writeNestedTree(cs.repo.repo.Storer, selected)
}

// StagedResyncTreeMatchesHead compares the pruned housekeeping tree, so removing
// a stale ignored import is a change even without new working content.
// Refs: MGIT-290, ADR-008 §3
func (cs *CommitStore) StagedResyncTreeMatchesHead() (bool, error) {
	return cs.treeMatchesHead(cs.buildResyncTree)
}

// CreateResyncCommit appends a housekeeping commit without obsolete ignored
// Git-untracked paths. Task commits must use CreateCommit instead.
// Refs: MGIT-290, ADR-008 §3
func (cs *CommitStore) CreateResyncCommit(_ context.Context, c *model.Commit) (string, error) {
	return cs.createCommit(c, cs.buildResyncTree)
}
