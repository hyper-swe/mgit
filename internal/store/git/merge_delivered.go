package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/hyper-swe/mgit/internal/model"
)

// ProjectionConflictError identifies a host path that must be preserved before a
// verified sandbox tip can be published. Refs: MGIT-317
type ProjectionConflictError struct {
	Path         string
	TargetExists bool
	TargetMode   model.FileDiffMode
	LinkTarget   string
}

func (e *ProjectionConflictError) Error() string {
	return fmt.Sprintf("native projection would overwrite host changes at %s", e.Path)
}

// FastForwardDelivered allows host entries already equal to the verified target
// (the host-edit/sandbox-sync workflow), as well as entries still at the old
// HEAD. It never accepts a third version. Contents AND git modes are compared,
// all paths are checked before writes, and untracked collisions are checked
// even when ignored by Status. Caller holds the shared repository process lock.
// Refs: MGIT-317
func (m *MergeStore) FastForwardDelivered(ctx context.Context, branch, targetHash string) error {
	refName := plumbing.NewBranchReferenceName(branch)
	cur, err := m.repo.repo.Storer.Reference(refName)
	if err != nil {
		return fmt.Errorf("%w: %s", model.ErrBranchNotFound, branch)
	}
	if cur.Hash().String() != targetHash {
		ok, e := m.IsAncestor(ctx, cur.Hash().String(), targetHash)
		if e != nil {
			return e
		}
		if !ok {
			return fmt.Errorf("native projection refused: non-fast-forward")
		}
	}
	current, err := m.repo.currentRef()
	if err != nil {
		return err
	}
	if current.Name() != refName {
		return fmt.Errorf("native projection view is not bound to branch %s", branch)
	}
	old, err := m.repo.headFiles()
	if err != nil {
		return err
	}
	commit, err := m.repo.repo.CommitObject(plumbing.NewHash(targetHash))
	if err != nil {
		return err
	}
	tree, err := commit.Tree()
	if err != nil {
		return err
	}
	target, err := flattenTree(tree)
	if err != nil {
		return err
	}
	staged, err := m.repo.stagedPaths()
	if err != nil {
		return err
	}
	if len(staged) > 0 {
		return m.projectionConflict(staged[0], target)
	}
	paths := make(map[string]bool, len(old)+len(target))
	for path := range old {
		paths[path] = true
	}
	for path := range target {
		paths[path] = true
	}
	for path := range paths {
		if err := validateRelPath(path); err != nil {
			return err
		}
		if err := m.repo.projectionParents(path); err != nil {
			var conflict *ProjectionConflictError
			if errors.As(err, &conflict) {
				return m.projectionConflict(conflict.Path, target)
			}
			return err
		}
		before, was := old[path]
		after, want := target[path]
		content, mode, e := m.repo.workingFileContent(path)
		if errors.Is(e, os.ErrNotExist) {
			if !was || !want {
				continue
			}
			return m.projectionConflict(path, target)
		}
		if e != nil {
			return e
		}
		actual := blobEntry{hash: plumbing.ComputeHash(plumbing.BlobObject, content), mode: mode}
		if (was && actual == before) || (want && actual == after) {
			continue
		}
		return m.projectionConflict(path, target)
	}
	if err := NewWorktreeStore(m.repo).materializeCommit(plumbing.NewHash(targetHash), old); err != nil {
		return err
	}
	return advanceBranchRefCAS(m.repo.repo.Storer, refName, plumbing.NewHash(targetHash), cur.Hash())
}

// projectionParents refuses a changed parent entry before either reading a
// descendant or materializing any path. Missing directories may be created;
// existing symlinks and non-directories are host conflicts, not destinations.
func (r *Repository) projectionParents(path string) error {
	parent := filepath.Dir(filepath.FromSlash(path))
	if parent == "." {
		return nil
	}
	relative := ""
	for _, part := range strings.Split(parent, string(filepath.Separator)) {
		relative = filepath.Join(relative, part)
		info, err := os.Lstat(filepath.Join(r.root, relative))
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return &ProjectionConflictError{Path: filepath.ToSlash(relative)}
		}
	}
	return nil
}

func (m *MergeStore) projectionConflict(path string, target map[string]blobEntry) error {
	conflict := &ProjectionConflictError{Path: path}
	entry, ok := target[path]
	if !ok {
		return conflict
	}
	conflict.TargetExists = true
	switch entry.mode {
	case filemode.Executable:
		conflict.TargetMode = model.FileModeExecutable
	case filemode.Symlink:
		conflict.TargetMode = model.FileModeSymlink
		data, err := m.repo.blobContent(entry.hash)
		if err != nil {
			return err
		}
		if len(data) > 4096 || strings.IndexByte(string(data), 0) >= 0 {
			return fmt.Errorf("native projection refused: invalid symlink target at %s", path)
		}
		conflict.LinkTarget = string(data)
	}
	return conflict
}
