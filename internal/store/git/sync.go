package git

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"

	"github.com/go-git/go-git/v5/plumbing"
)

// This file is the store-side support for ADR-008 auto-housekeeping: a cheap,
// CONTENT-BASED working-tree fingerprint (the drift signal — never mtime, which
// false-negatives), and a check of whether the staged tree already matches the
// base so a resync can no-op rather than append an empty commit.
// Refs: MGIT-35, ADR-008 §3

// WorkingTreeFingerprint returns a content hash over every trackable working
// file (path + blob hash + mode), honoring .gitignore and excluding .mgit/.git.
// It is the local-working-state half of the drift signal: any content change to
// a tracked or untracked-but-trackable file changes the fingerprint, so the
// gate never false-negatives on a same-size/same-mtime edit. The cost is one
// blob-hash per working file — the same scan `mgit status` already performs —
// while the expensive resync (blob import + tree build + commit) runs ONLY when
// the fingerprint actually differs. Refs: MGIT-35, ADR-008 §3
func (r *Repository) WorkingTreeFingerprint() (string, error) {
	paths, err := r.listWorkingFiles()
	if err != nil {
		return "", fmt.Errorf("working-tree fingerprint: %w", err)
	}
	sort.Strings(paths)
	h := sha256.New()
	for _, rel := range paths {
		data, mode, err := r.workingFileContent(rel)
		if err != nil {
			return "", fmt.Errorf("working-tree fingerprint: %s: %w", rel, err)
		}
		// Hash a stable record per file: path, mode, and a content digest. Using
		// the content digest (not raw bytes) keeps the rolling hash cheap and
		// order-independent issues moot because paths are sorted first.
		blob := sha256.Sum256(data)
		fmt.Fprintf(h, "%s\x00%o\x00%x\n", rel, mode, blob)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// PathsMatchingCommitted returns the subset of paths whose CURRENT working-tree
// content is byte-identical to the content git has COMMITTED for that path
// (committed maps project-relative path -> git blob id, from
// gitref.CommittedBlobs). A path absent from committed is untracked in git, and
// a path whose content differs is locally modified: both are the user's
// UNCOMMITTED work, and neither may be absorbed into the base by a read verb's
// auto-resync — absorbing them is what made a user's edit uncommittable and
// untraceable in MGIT-123. Comparison is by git blob id, so it is exact and
// case-preserving. Refs: MGIT-123, ADR-008 §3
func (r *Repository) PathsMatchingCommitted(committed map[string]string, paths []string) ([]string, error) {
	var out []string
	for _, rel := range paths {
		want, tracked := committed[rel]
		if !tracked {
			continue // untracked in git → uncommitted user work
		}
		data, _, err := r.workingFileContent(rel)
		if err != nil {
			return nil, fmt.Errorf("compare %s against git-committed content: %w", rel, err)
		}
		if plumbing.ComputeHash(plumbing.BlobObject, data).String() == want {
			out = append(out, rel)
		}
	}
	return out, nil
}

// ClearStaging resets the staging area. It is the exported entry point the
// resync routine uses to discard a no-op staging set without committing it.
// Refs: MGIT-35
func (r *Repository) ClearStaging() error {
	return r.clearStaging()
}

// StagedTreeMatchesHead reports whether building a commit from HEAD plus the
// current staging set would produce a tree identical to the current HEAD tree.
// The resync uses it to AVOID appending an empty base commit when nothing
// actually changed (idempotence): if the staged tree equals HEAD's tree, the
// resync records the new fingerprint and skips the commit. Refs: MGIT-35, ADR-008 §3
func (cs *CommitStore) StagedTreeMatchesHead() (bool, error) {
	headRef, err := cs.repo.currentRef()
	if err != nil {
		return false, fmt.Errorf("resync compare: resolve HEAD: %w", err)
	}
	headCommit, err := cs.repo.repo.CommitObject(headRef.Hash())
	if err != nil {
		return false, fmt.Errorf("resync compare: load HEAD commit: %w", err)
	}
	staged, err := cs.buildTreeFromStaging()
	if err != nil {
		return false, fmt.Errorf("resync compare: build staged tree: %w", err)
	}
	return headCommit.TreeHash == staged, nil
}
