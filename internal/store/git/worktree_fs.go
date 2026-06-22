package git

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
)

// This file is the bridge between mgit's bare .mgit/ object store and the
// project's working files on disk. Because the store has NO go-git worktree,
// mgit reads and writes project files itself, rooted at Repository.Root(), and
// always EXCLUDES .mgit/ (mgit's own store) and .git/ (the project's git repo,
// which is sacrosanct and must never be read or written by mgit).
// Refs: MGIT-14.3, MGIT-14.4, ADR-001 (amendment 2026-06-22)

// excludedRoots are top-level directories mgit must never traverse or touch:
// its own store and the project's git repository.
var excludedRoots = map[string]bool{
	mgitDirName: true,
	".git":      true,
}

// headFiles returns a flat map of every file path → blob hash tracked at HEAD,
// recursing into subtrees. This is the baseline for status and commit-building.
func (r *Repository) headFiles() (map[string]plumbing.Hash, error) {
	headRef, err := r.repo.Head()
	if err != nil {
		return nil, fmt.Errorf("resolve HEAD: %w", err)
	}
	commit, err := r.repo.CommitObject(headRef.Hash())
	if err != nil {
		return nil, fmt.Errorf("get HEAD commit: %w", err)
	}
	tree, err := commit.Tree()
	if err != nil {
		return nil, fmt.Errorf("get HEAD tree: %w", err)
	}
	return flattenTree(tree)
}

// readWorkingFile reads a single project file by its project-relative path,
// rejecting paths that escape the root or reach into excluded directories.
func (r *Repository) readWorkingFile(rel string) ([]byte, error) {
	if err := validateRelPath(rel); err != nil {
		return nil, err
	}
	abs := filepath.Join(r.root, filepath.FromSlash(rel))
	data, err := os.ReadFile(abs) //nolint:gosec // path validated against root and excluded dirs
	if err != nil {
		return nil, fmt.Errorf("read working file %s: %w", rel, err)
	}
	return data, nil
}

// listWorkingFiles walks the project root and returns the sorted set of
// project-relative file paths, excluding .mgit/ and .git/. Directories are not
// included; symlinks are followed only as plain files by os.ReadFile callers.
func (r *Repository) listWorkingFiles() ([]string, error) {
	var paths []string
	err := filepath.WalkDir(r.root, func(abs string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(r.root, abs)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		top := strings.SplitN(rel, "/", 2)[0]
		if excludedRoots[top] {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		paths = append(paths, rel)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk working tree: %w", err)
	}
	return paths, nil
}

// blobHashOfWorkingFile reads a working file and returns the git blob hash its
// content would have, WITHOUT storing it. Used by Status to detect changes.
func (r *Repository) blobHashOfWorkingFile(rel string) (plumbing.Hash, error) {
	data, err := r.readWorkingFile(rel)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	obj := &plumbing.MemoryObject{}
	obj.SetType(plumbing.BlobObject)
	if _, err := obj.Write(data); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("hash working file %s: %w", rel, err)
	}
	return obj.Hash(), nil
}

// writeBlobToDisk reads the blob object at blobHash from the .mgit store and
// writes its content to the working file at the given project-relative path,
// creating parent directories as needed. Used by checkout materialization.
func (r *Repository) writeBlobToDisk(rel string, blobHash plumbing.Hash) error {
	if err := validateRelPath(rel); err != nil {
		return err
	}
	blob, err := r.repo.BlobObject(blobHash)
	if err != nil {
		return fmt.Errorf("load blob %s for %s: %w", blobHash, rel, err)
	}
	reader, err := blob.Reader()
	if err != nil {
		return fmt.Errorf("read blob %s: %w", blobHash, err)
	}
	defer reader.Close() //nolint:errcheck // best-effort close after copy
	data, err := io.ReadAll(reader)
	if err != nil {
		return fmt.Errorf("read blob %s: %w", blobHash, err)
	}
	abs := filepath.Join(r.root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o750); err != nil {
		return fmt.Errorf("mkdir for %s: %w", rel, err)
	}
	if err := os.WriteFile(abs, data, 0o600); err != nil {
		return fmt.Errorf("write working file %s: %w", rel, err)
	}
	return nil
}

// validateRelPath rejects empty, absolute, parent-escaping, or excluded paths.
func validateRelPath(rel string) error {
	if rel == "" {
		return fmt.Errorf("path must not be empty")
	}
	clean := filepath.ToSlash(filepath.Clean(rel))
	if filepath.IsAbs(rel) || strings.HasPrefix(clean, "../") || clean == ".." {
		return fmt.Errorf("path escapes project root: %s", rel)
	}
	top := strings.SplitN(clean, "/", 2)[0]
	if excludedRoots[top] {
		return fmt.Errorf("path is in an excluded directory: %s", rel)
	}
	return nil
}
