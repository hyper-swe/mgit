package git

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"

	"github.com/hyper-swe/mgit/internal/model"
)

// DirtyPaths reports which of the given project-relative paths carry
// uncommitted local state: staged in mgit's staging area, edited on disk
// relative to the current branch tip, deleted on disk while tracked, or
// present on disk while untracked. Content-applying verbs (rollback,
// cherry-pick — MGIT-54) refuse to touch dirty paths so they never clobber
// in-flight work. Mode-only changes are not treated as dirty.
// Refs: MGIT-54, FR-6
func (r *Repository) DirtyPaths(paths []string) ([]string, error) {
	staged, err := r.stagedPaths()
	if err != nil {
		return nil, fmt.Errorf("dirty check: read staging: %w", err)
	}
	stagedSet := make(map[string]bool, len(staged))
	for _, p := range staged {
		stagedSet[p] = true
	}

	head, err := r.headFiles()
	if err != nil {
		return nil, fmt.Errorf("dirty check: read HEAD tree: %w", err)
	}

	var dirty []string
	for _, p := range paths {
		rel := filepath.ToSlash(p)
		if stagedSet[rel] {
			dirty = append(dirty, rel)
			continue
		}
		entry, tracked := head[rel]
		content, _, readErr := r.workingFileContent(rel)
		switch {
		case errors.Is(readErr, os.ErrNotExist):
			if tracked {
				dirty = append(dirty, rel) // deleted locally, uncommitted
			}
		case readErr != nil:
			return nil, fmt.Errorf("dirty check: read %s: %w", rel, readErr)
		case !tracked:
			dirty = append(dirty, rel) // untracked file occupying the path
		default:
			if plumbing.ComputeHash(plumbing.BlobObject, content) != entry.hash {
				dirty = append(dirty, rel) // edited locally, uncommitted
			}
		}
	}
	return dirty, nil
}

// ComputeRestoreDiffs compares the WORKING DIRECTORY (not the committed
// tree) against the target commit's tree and returns the diffs that
// transform disk into the checkpoint state: writes for paths whose disk
// content/mode differ from the target, deletions for paths tracked at the
// current tip but absent from the target. Untracked files absent from both
// are left alone. It also reports which of those paths carry UNCOMMITTED
// local state (staged, or disk differing from the current tip), so the
// caller can refuse or require an explicit force — this is what makes
// `restore --all` able to recover a trashed-but-uncommitted tree instead of
// no-opping on it. Refs: MGIT-55 (review finding M1)
func (r *Repository) ComputeRestoreDiffs(targetHash string) (apply []model.FileDiff, uncommitted []string, err error) {
	targetCommit, err := r.repo.CommitObject(plumbing.NewHash(targetHash))
	if err != nil {
		return nil, nil, fmt.Errorf("restore diffs: load target %s: %w", targetHash, err)
	}
	targetTree, err := targetCommit.Tree()
	if err != nil {
		return nil, nil, fmt.Errorf("restore diffs: target tree: %w", err)
	}
	target, err := flattenTree(targetTree)
	if err != nil {
		return nil, nil, fmt.Errorf("restore diffs: flatten target: %w", err)
	}
	head, err := r.headFiles()
	if err != nil {
		return nil, nil, err
	}
	staged, err := r.stagedPaths()
	if err != nil {
		return nil, nil, fmt.Errorf("restore diffs: read staging: %w", err)
	}
	stagedSet := make(map[string]bool, len(staged))
	for _, p := range staged {
		stagedSet[p] = true
	}

	union := make(map[string]bool, len(target)+len(head))
	for p := range target {
		union[p] = true
	}
	for p := range head {
		union[p] = true
	}
	paths := make([]string, 0, len(union))
	for p := range union {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	for _, rel := range paths {
		diskHash, diskMode, onDisk, readErr := r.workingBlobState(rel)
		if readErr != nil {
			return nil, nil, fmt.Errorf("restore diffs: read %s: %w", rel, readErr)
		}
		want, inTarget := target[rel]
		headEntry, inHead := head[rel]

		var d *model.FileDiff
		switch {
		case inTarget && (!onDisk || diskHash != want.hash || diskMode != want.mode):
			op := model.DiffModified
			if !onDisk {
				op = model.DiffAdded
			}
			d = &model.FileDiff{Path: rel, Operation: op, NewHash: want.hash.String(), Mode: diffModeFromGit(want.mode)}
		case !inTarget && onDisk && inHead:
			d = &model.FileDiff{Path: rel, Operation: model.DiffDeleted, OldHash: diskHash.String()}
		}
		if d == nil {
			continue
		}
		apply = append(apply, *d)
		// Uncommitted = the path's disk state diverges from the current tip
		// (or it is explicitly staged): restoring it would overwrite local work.
		diskMatchesHead := (onDisk && inHead && diskHash == headEntry.hash) || (!onDisk && !inHead)
		if stagedSet[rel] || !diskMatchesHead {
			uncommitted = append(uncommitted, rel)
		}
	}
	return apply, uncommitted, nil
}

// workingBlobState reports a working file's blob hash and git mode, with
// onDisk=false (and no error) for an absent path. Refs: MGIT-55
func (r *Repository) workingBlobState(rel string) (plumbing.Hash, filemode.FileMode, bool, error) {
	content, mode, err := r.workingFileContent(rel)
	if errors.Is(err, os.ErrNotExist) {
		return plumbing.ZeroHash, 0, false, nil
	}
	if err != nil {
		return plumbing.ZeroHash, 0, false, err
	}
	return plumbing.ComputeHash(plumbing.BlobObject, content), mode, true, nil
}

// IsAncestorOfHead reports whether the commit is an ancestor of (or equal to)
// the current branch tip. Rollback nets only ancestor commits: a task's
// squash artifact lives on its task/<id> branch, not this lineage, and must
// not be double-counted into the revert. Refs: MGIT-54, MGIT-22
func (r *Repository) IsAncestorOfHead(hash string) (bool, error) {
	headRef, err := r.currentRef()
	if err != nil {
		return false, fmt.Errorf("resolve HEAD: %w", err)
	}
	if headRef.Hash().String() == hash {
		return true, nil
	}
	c, err := r.repo.CommitObject(plumbing.NewHash(hash))
	if err != nil {
		return false, fmt.Errorf("load commit %s: %w", hash, err)
	}
	head, err := r.repo.CommitObject(headRef.Hash())
	if err != nil {
		return false, fmt.Errorf("load HEAD commit: %w", err)
	}
	ok, err := c.IsAncestor(head)
	if err != nil {
		return false, fmt.Errorf("ancestry check %s: %w", hash, err)
	}
	return ok, nil
}

// MaterializeDiffs applies the given (already-committed) diffs to the working
// directory: added/modified entries are written from their blobs with git
// modes preserved, deleted entries are removed from disk. Only the diffed
// paths are touched — unrelated working files (including uncommitted work)
// are never rewritten, unlike a full-tree checkout. All paths are validated
// before anything is written. Refs: MGIT-54
func (r *Repository) MaterializeDiffs(diffs []model.FileDiff) error {
	for _, d := range diffs {
		if err := validateRelPath(filepath.ToSlash(d.Path)); err != nil {
			return fmt.Errorf("materialize: invalid path %s: %w", d.Path, err)
		}
	}
	for _, d := range diffs {
		rel := filepath.ToSlash(d.Path)
		if d.Operation == model.DiffDeleted {
			abs := filepath.Join(r.root, filepath.FromSlash(rel))
			if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("materialize: remove %s: %w", rel, err)
			}
			continue
		}
		entry := blobEntry{hash: plumbing.NewHash(d.NewHash), mode: gitModeFromDiff(d.Mode)}
		if err := r.writeEntryToDisk(rel, entry); err != nil {
			return fmt.Errorf("materialize: %w", err)
		}
	}
	return nil
}
