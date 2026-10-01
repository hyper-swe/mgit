package git

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/hyper-swe/mgit/internal/model"
)

// isDirectoryTarget reports whether an `add` argument names a directory: one
// on disk (a symlink is a file to git, never a directory), or one that is
// gone but held tracked files, whose deletions it then stages. A path tracked
// as a file itself is never a directory target. Refs: MGIT-276
func (ws *WorktreeStore) isDirectoryTarget(rel string) (bool, error) {
	abs := filepath.Join(ws.repo.root, filepath.FromSlash(rel))
	info, err := os.Lstat(abs)
	if err == nil {
		return info.IsDir(), nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	head, err := ws.repo.headFiles()
	if err != nil {
		return false, err
	}
	if _, tracked := head[rel]; tracked {
		return false, nil
	}
	return hasPathUnder(head, rel), nil
}

// addChanged stages every file that differs from HEAD (changed, untracked or
// deleted) under prefix — the whole project when prefix is empty, which is
// `add -A` — and never a directory.
//
// A directory given to `mgit add` used to be staged AS ONE PATH, after which
// every commit failed reading it as a file and nothing could unstage it
// (MGIT-276). Expanding it the way git does is the same walk as `add -A`, so
// it shares that walk's rules: ignored paths are absent from Status, mgit's
// own scaffolding is skipped by recorded provenance (MGIT-80), and the size
// tripwire weighs what would be staged (MGIT-131). An unchanged or empty
// directory stages nothing, as in git. Refs: MGIT-276, MGIT-80, MGIT-77, MGIT-131
func (ws *WorktreeStore) addChanged(ctx context.Context, prefix, label string) error {
	files, err := ws.Status(ctx)
	if err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	generated, err := ws.repo.generatedSet()
	if err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	paths := make([]string, 0, len(files))
	for _, f := range files {
		if strings.HasPrefix(f.Path, mgitDirName+"/") || f.Path == mgitDirName || generated[f.Path] {
			continue
		}
		if prefix == "" || strings.HasPrefix(f.Path, prefix+"/") {
			paths = append(paths, f.Path)
		}
	}
	if len(paths) == 0 {
		return nil
	}
	if err := ws.assertNotOversized(paths); err != nil {
		return err
	}
	if err := ws.repo.stagePaths(paths); err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	return nil
}

// hasPathUnder reports whether any tracked path lies under dir.
func hasPathUnder(head map[string]blobEntry, dir string) bool {
	for path := range head {
		if strings.HasPrefix(path, dir+"/") {
			return true
		}
	}
	return false
}

// checkStagedEntry refuses a staged path that a commit cannot record as one
// path: a directory on disk, or a gone directory that held tracked files. A
// staging file written before MGIT-276 can still name one. Before this check,
// commit failed "read working file …: is a directory" — or, for a gone
// directory, silently recorded nothing for the files under it. The refusal
// names the entry and the way out. Refs: MGIT-276
func (r *Repository) checkStagedEntry(rel string, head map[string]blobEntry) error {
	abs := filepath.Join(r.root, filepath.FromSlash(rel))
	info, err := os.Lstat(abs)
	isDir := err == nil && info.IsDir()
	if errors.Is(err, fs.ErrNotExist) {
		_, tracked := head[rel]
		isDir = !tracked && hasPathUnder(head, rel)
	}
	if !isDir {
		return nil
	}
	return fmt.Errorf("%w: %q is a directory, which a commit cannot record as one path; "+
		"run `mgit restore --staged %s`, then `mgit add %s` to stage the files under it",
		model.ErrInvalidStagedEntry, rel, rel, rel)
}

// Unstage removes paths from the staging area and returns the entries it
// removed, leaving every other entry staged. A directory argument removes its
// own entry, if a staging file holds one, and every entry under it; "."
// removes everything. A path that is not staged removes nothing. Nothing on
// disk changes. Refs: MGIT-276
func (r *Repository) Unstage(rels []string) ([]string, error) {
	targets := make([]string, 0, len(rels))
	for _, p := range rels {
		rel := filepath.ToSlash(filepath.Clean(p))
		if rel != "." {
			if err := validateRelPath(rel); err != nil {
				return nil, fmt.Errorf("unstage %s: %w", p, err)
			}
		}
		targets = append(targets, rel)
	}
	s, err := r.loadStaging()
	if err != nil {
		return nil, err
	}
	var kept, removed []string
	for _, path := range s.Paths {
		if matchesAnyTarget(path, targets) {
			removed = append(removed, path)
		} else {
			kept = append(kept, path)
		}
	}
	if len(removed) == 0 {
		return nil, nil
	}
	if len(kept) == 0 {
		return removed, r.clearStaging()
	}
	s.Paths = kept
	return removed, r.saveStaging(s)
}

// matchesAnyTarget reports whether a staged path is one of the targets or
// lies under one.
func matchesAnyTarget(path string, targets []string) bool {
	for _, t := range targets {
		if t == "." || path == t || strings.HasPrefix(path, t+"/") {
			return true
		}
	}
	return false
}
