package git

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/hyper-swe/mgit/internal/model"
)

// Single-character git-style status codes used in FileStatus.Staging and
// FileStatus.Worktree. Centralized so status semantics live in one place.
// Refs: FR-8, MGIT-2.2.6
const (
	statusUnmodified = " "
	statusUntracked  = "?"
	statusAdded      = "A"
	statusModified   = "M"
	statusDeleted    = "D"
)

// FileStatus represents the status of a file in the worktree.
// Staging/Worktree are single-character git-style codes (see status* consts):
// unmodified, untracked, added (staged new), modified, deleted.
// Refs: FR-8 (status command), MGIT-2.2.6, MGIT-14.3
type FileStatus struct {
	Path     string `json:"path"`
	Staging  string `json:"staging"`
	Worktree string `json:"worktree"`
}

// WorktreeStore provides worktree operations (status, add, checkout, clean)
// against mgit's self-contained .mgit/ store. It owns its staging model and
// reads/writes project files directly via the Repository root — it never uses
// a go-git worktree or the project's `.git`.
// Refs: FR-2, FR-8, MGIT-2.2.6, MGIT-14.3, MGIT-14.4
type WorktreeStore struct {
	repo *Repository
}

// NewWorktreeStore creates a WorktreeStore backed by the given Repository.
func NewWorktreeStore(repo *Repository) *WorktreeStore {
	return &WorktreeStore{repo: repo}
}

// IsClean reports whether the user-visible portion of the worktree has
// no uncommitted changes. Files under the .mgit/ directory are excluded
// because they hold mgit's own object store and SQLite index, which are
// not user content. When the second return value is non-empty, it lists
// the dirty user-visible paths that are blocking a clean state.
// Refs: FR-5.5a, MGIT-4.2.9
func (ws *WorktreeStore) IsClean(ctx context.Context) (bool, []string, error) {
	files, err := ws.Status(ctx)
	if err != nil {
		return false, nil, err
	}
	var dirty []string
	for _, f := range files {
		if strings.HasPrefix(f.Path, mgitDirName+"/") || f.Path == mgitDirName {
			continue
		}
		// Any non-unmodified flag in either staging or worktree counts as dirty.
		if f.Staging != " " && f.Staging != "" || f.Worktree != " " && f.Worktree != "" {
			dirty = append(dirty, f.Path)
		}
	}
	return len(dirty) == 0, dirty, nil
}

// dirtyTrackedPaths returns user-visible paths whose tracked content carries
// uncommitted changes that a checkout would CLOBBER: a worktree modification of
// a tracked file, or any staged change. Untracked files are excluded (checkout
// neither removes nor overwrites them); a pure worktree deletion is also
// excluded, because restoring a tracked file the user deleted loses no
// uncommitted content — matching git, which silently restores it on switch.
// This is the overwrite-protection signal for the store-level checkout guard.
// Refs: MGIT-14.7 (#4)
func (ws *WorktreeStore) dirtyTrackedPaths(ctx context.Context) ([]string, error) {
	files, err := ws.Status(ctx)
	if err != nil {
		return nil, err
	}
	var dirty []string
	for _, f := range files {
		// Status never emits a .mgit/ path (listWorkingFiles excludes it and the
		// HEAD tree never contains it), so no .mgit filtering is needed here.
		staged := f.Staging != statusUnmodified
		if staged || f.Worktree == statusModified {
			dirty = append(dirty, f.Path)
		}
	}
	return dirty, nil
}

// Status returns the status of all user-visible files by comparing the working
// tree on disk against the HEAD tree and the mgit-owned staging set. It is
// computed purely via plumbing + filesystem reads (no go-git index/worktree).
// Refs: FR-8, MGIT-14.3
func (ws *WorktreeStore) Status(_ context.Context) ([]FileStatus, error) {
	head, err := ws.repo.headFiles()
	if err != nil {
		return nil, err
	}
	working, err := ws.repo.listWorkingFiles()
	if err != nil {
		return nil, err
	}
	staged, err := ws.repo.stagedPaths()
	if err != nil {
		return nil, err
	}
	stagedSet := make(map[string]bool, len(staged))
	for _, p := range staged {
		stagedSet[p] = true
	}

	files := make([]FileStatus, 0)
	seen := make(map[string]bool, len(working))
	for _, path := range working {
		seen[path] = true
		fsStatus, err := ws.classifyWorkingPath(path, head, stagedSet)
		if err != nil {
			return nil, err
		}
		if fsStatus != nil {
			files = append(files, *fsStatus)
		}
	}

	// Paths present in HEAD but absent on disk are deletions.
	for path := range head {
		if seen[path] {
			continue
		}
		st := FileStatus{Path: path, Staging: statusUnmodified, Worktree: statusDeleted}
		if stagedSet[path] {
			st.Staging = statusDeleted
		}
		files = append(files, st)
	}
	return files, nil
}

// classifyWorkingPath returns the status of a single on-disk path, or nil when
// the path is unmodified relative to HEAD and not staged.
func (ws *WorktreeStore) classifyWorkingPath(path string, head map[string]blobEntry, stagedSet map[string]bool) (*FileStatus, error) {
	headEntry, inHead := head[path]
	wtHash, err := ws.repo.blobHashOfWorkingFile(path)
	if err != nil {
		return nil, err
	}
	changed := !inHead || headEntry.hash != wtHash

	switch {
	case stagedSet[path]:
		// Staged (whether or not it differs from HEAD): callers that treat any
		// staged path as dirty (checkout guard) must see a non-clean flag. A
		// never-tracked staged path is "A"; an already-tracked one is "M".
		staging := statusModified
		if !inHead {
			staging = statusAdded
		}
		return &FileStatus{Path: path, Staging: staging, Worktree: statusUnmodified}, nil
	case !inHead:
		return &FileStatus{Path: path, Staging: statusUnmodified, Worktree: statusUntracked}, nil
	case changed:
		return &FileStatus{Path: path, Staging: statusUnmodified, Worktree: statusModified}, nil
	default:
		return nil, nil
	}
}

// Add stages files for the next commit. A path of "." stages every changed and
// untracked user-visible file; otherwise the given project-relative path is
// staged. Staging is recorded in mgit's own staging file under .mgit/.
// Refs: FR-2, MGIT-14.3
func (ws *WorktreeStore) Add(ctx context.Context, path string) error {
	if path == "." || path == "" {
		return ws.addAll(ctx)
	}
	rel := filepath.ToSlash(filepath.Clean(path))
	if err := validateRelPath(rel); err != nil {
		return fmt.Errorf("add %s: %w", path, err)
	}
	// A path may be staged only if it exists on disk (add/modify) or is tracked
	// in HEAD (staging a deletion); otherwise it is an error, like `git add` of
	// a nonexistent, never-tracked path.
	if err := ws.assertStageable(rel); err != nil {
		return fmt.Errorf("add %s: %w", path, err)
	}
	if err := ws.repo.stagePath(rel); err != nil {
		return fmt.Errorf("add %s: %w", path, err)
	}
	return nil
}

// assertStageable verifies a path can be staged: it must exist on disk or be a
// file tracked in the HEAD tree (the latter allows staging a deletion).
func (ws *WorktreeStore) assertStageable(rel string) error {
	abs := filepath.Join(ws.repo.root, filepath.FromSlash(rel))
	if _, statErr := os.Stat(abs); statErr == nil {
		return nil
	}
	head, err := ws.repo.headFiles()
	if err != nil {
		return err
	}
	if _, tracked := head[rel]; tracked {
		return nil
	}
	return fmt.Errorf("pathspec did not match any files: %s", rel)
}

// addAll stages every file that differs from HEAD (changed, untracked, or
// deleted), mirroring `git add -A`.
func (ws *WorktreeStore) addAll(ctx context.Context) error {
	files, err := ws.Status(ctx)
	if err != nil {
		return fmt.Errorf("add all: %w", err)
	}
	paths := make([]string, 0, len(files))
	for _, f := range files {
		if strings.HasPrefix(f.Path, mgitDirName+"/") || f.Path == mgitDirName {
			continue
		}
		paths = append(paths, f.Path)
	}
	if err := ws.repo.stagePaths(paths); err != nil {
		return fmt.Errorf("add all: %w", err)
	}
	return nil
}

// Checkout switches HEAD to the named branch and MATERIALIZES that branch's
// tree onto disk via plumbing: every blob in the branch tree is written to the
// project root, and tracked files no longer present in the target tree are
// removed. It never uses a go-git worktree. Returns ErrBranchNotFound if the
// branch does not exist, or ErrRollbackConflict if the worktree has
// uncommitted user changes (a store-level clean-or-fail guard, defense in depth
// over the service layer's IsClean check — #4). Refs: FR-5, FR-8, MGIT-14.4, MGIT-14.7
func (ws *WorktreeStore) Checkout(ctx context.Context, branchName string) error {
	refName := plumbing.NewBranchReferenceName(branchName)
	ref, err := ws.repo.repo.Storer.Reference(refName)
	if err != nil {
		return fmt.Errorf("%w: %s", model.ErrBranchNotFound, branchName)
	}

	// Defense in depth: refuse to clobber uncommitted changes to TRACKED content
	// even when called directly (the service layer also guards with IsClean).
	// Untracked files are not at risk — materialize never removes them and only
	// overwrites tracked paths — so they do not block, matching git's checkout.
	dirty, err := ws.dirtyTrackedPaths(ctx)
	if err != nil {
		return fmt.Errorf("checkout %s: %w", branchName, err)
	}
	if len(dirty) > 0 {
		return fmt.Errorf("%w: checkout would overwrite uncommitted changes: %v", model.ErrRollbackConflict, dirty)
	}

	// Flatten the current HEAD tree once here; materializeCommit reuses it for
	// its deletion pass rather than re-reading and re-flattening HEAD.
	currentHead, err := ws.repo.headFiles()
	if err != nil {
		return fmt.Errorf("checkout %s: %w", branchName, err)
	}
	if err := ws.materializeCommit(ref.Hash(), currentHead); err != nil {
		return fmt.Errorf("checkout %s: %w", branchName, err)
	}

	// Point HEAD at the branch and reset staging — the working tree now matches.
	headRef := plumbing.NewSymbolicReference(plumbing.HEAD, refName)
	if err := ws.repo.repo.Storer.SetReference(headRef); err != nil {
		return fmt.Errorf("checkout %s: set HEAD: %w", branchName, err)
	}
	if err := ws.repo.clearStaging(); err != nil {
		return fmt.Errorf("checkout %s: %w", branchName, err)
	}
	return nil
}

// materializeCommit writes the tree of the given commit onto the project root
// (preserving each entry's git mode — executable bit and symlinks) and removes
// tracked files that are absent from the target tree. Untracked files (never
// committed) are left in place. ALL target paths are validated up front, before
// any file is written, so a single crafted/escaping path cannot leave a partial
// checkout on disk (#7). currentHead is the caller's already-flattened HEAD
// tree, used for the deletion pass. Refs: MGIT-14.4, MGIT-14.7
func (ws *WorktreeStore) materializeCommit(commitHash plumbing.Hash, currentHead map[string]blobEntry) error {
	commit, err := ws.repo.repo.CommitObject(commitHash)
	if err != nil {
		return fmt.Errorf("load commit %s: %w", commitHash, err)
	}
	tree, err := commit.Tree()
	if err != nil {
		return fmt.Errorf("load tree for %s: %w", commitHash, err)
	}
	target, err := flattenTree(tree)
	if err != nil {
		return fmt.Errorf("flatten target tree: %w", err)
	}

	// Validate every target path BEFORE mutating disk: an invalid (escaping or
	// excluded) path aborts the whole checkout with nothing written.
	for path := range target {
		if err := validateRelPath(path); err != nil {
			return fmt.Errorf("invalid target path %s: %w", path, err)
		}
	}

	for path, entry := range target {
		if err := ws.repo.writeEntryToDisk(path, entry); err != nil {
			return err
		}
	}

	// Remove files that were tracked at the old HEAD but not in the target tree.
	for path := range currentHead {
		if _, ok := target[path]; ok {
			continue
		}
		abs := filepath.Join(ws.repo.root, filepath.FromSlash(path))
		if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove %s: %w", path, err)
		}
	}
	return nil
}

// MaterializeBranchTo writes the tree of branchName's tip commit into destRoot
// — a SEPARATE filesystem root (a linked worktree's path), not the project
// root. It is the materialization `mgit worktree add` was missing (MGIT-17):
// every blob in the branch tree is written under destRoot with its git mode
// preserved, so the linked worktree is a usable working copy rather than an
// empty dir. Unlike Checkout/materializeCommit it does NOT switch HEAD, run a
// deletion pass (the destination is a fresh worktree), or touch the project's
// .git — it only reads the .mgit object store and writes plain files under
// destRoot. Every relative path is validated up front so nothing can escape
// destRoot. Returns ErrBranchNotFound if the branch does not exist.
// Refs: FR-16, MGIT-17
func (ws *WorktreeStore) MaterializeBranchTo(_ context.Context, branchName, destRoot string) error {
	refName := plumbing.NewBranchReferenceName(branchName)
	ref, err := ws.repo.repo.Storer.Reference(refName)
	if err != nil {
		return fmt.Errorf("%w: %s", model.ErrBranchNotFound, branchName)
	}
	commit, err := ws.repo.repo.CommitObject(ref.Hash())
	if err != nil {
		return fmt.Errorf("materialize branch %s: load commit: %w", branchName, err)
	}
	tree, err := commit.Tree()
	if err != nil {
		return fmt.Errorf("materialize branch %s: load tree: %w", branchName, err)
	}
	target, err := flattenTree(tree)
	if err != nil {
		return fmt.Errorf("materialize branch %s: flatten tree: %w", branchName, err)
	}

	// Validate every path BEFORE writing anything: one escaping path aborts the
	// whole materialization with nothing written.
	for path := range target {
		if err := validateRelPath(path); err != nil {
			return fmt.Errorf("materialize branch %s: invalid path %s: %w", branchName, path, err)
		}
	}
	if err := os.MkdirAll(destRoot, 0o750); err != nil {
		return fmt.Errorf("materialize branch %s: mkdir %s: %w", branchName, destRoot, err)
	}
	for path, entry := range target {
		if err := ws.repo.writeEntryToDir(destRoot, path, entry); err != nil {
			return fmt.Errorf("materialize branch %s: %w", branchName, err)
		}
	}

	// Carry gitignored-but-build-required artifacts (e.g. a generated web/dist
	// that a //go:embed depends on) listed in .mgit/seed-include. These are
	// absent from the materialized tree because mgit's add honors .gitignore
	// (MGIT-32), so they are copied from the live source working tree AFTER the
	// tree write — without importing them into .mgit (the base/audit stays
	// clean). Everything NOT listed stays excluded. Refs: MGIT-38
	if err := ws.repo.copySeedIncludes(destRoot); err != nil {
		return fmt.Errorf("materialize branch %s: seed-include: %w", branchName, err)
	}
	return nil
}

// Clean removes untracked user-visible files from the working tree (files not
// present in the HEAD tree), mirroring `git clean -d`. It never touches .mgit/
// or the project .git.
// Refs: FR-8, MGIT-14.4
func (ws *WorktreeStore) Clean(ctx context.Context) error {
	files, err := ws.Status(ctx)
	if err != nil {
		return fmt.Errorf("clean: %w", err)
	}
	for _, f := range files {
		if f.Worktree != statusUntracked {
			continue
		}
		abs := filepath.Join(ws.repo.root, filepath.FromSlash(f.Path))
		if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("clean %s: %w", f.Path, err)
		}
	}
	return nil
}
