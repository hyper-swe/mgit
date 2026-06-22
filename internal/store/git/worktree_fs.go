package git

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
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

// headFiles returns a flat map of every file path → blobEntry (hash + mode)
// tracked at HEAD, recursing into subtrees. This is the baseline for status
// and commit-building; the mode is carried so commits and checkouts preserve
// git's regular/executable/symlink distinction. Refs: MGIT-14.7
func (r *Repository) headFiles() (map[string]blobEntry, error) {
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

// workingFileContent reads a single project file by its project-relative path
// and returns the bytes that should be stored as its git blob together with the
// git file mode to record for it. It uses os.Lstat (NOT os.Stat) so a symlink
// is detected rather than dereferenced: for a symlink the returned content is
// the LINK TEXT (the target path string) and the mode is 120000 — git-faithful,
// and it prevents committing an out-of-tree target's content as a regular blob
// (the exfiltration defect). Regular files return their content with mode
// 100644, or 100755 when the owner-executable bit is set. Refs: MGIT-14.7 (#2, #3)
func (r *Repository) workingFileContent(rel string) ([]byte, filemode.FileMode, error) {
	if err := validateRelPath(rel); err != nil {
		return nil, filemode.Empty, err
	}
	abs := filepath.Join(r.root, filepath.FromSlash(rel))
	info, err := os.Lstat(abs)
	if err != nil {
		return nil, filemode.Empty, fmt.Errorf("stat working file %s: %w", rel, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, rerr := os.Readlink(abs)
		if rerr != nil {
			return nil, filemode.Empty, fmt.Errorf("read symlink %s: %w", rel, rerr)
		}
		return []byte(target), filemode.Symlink, nil
	}
	data, err := os.ReadFile(abs) //nolint:gosec // path validated against root and excluded dirs
	if err != nil {
		return nil, filemode.Empty, fmt.Errorf("read working file %s: %w", rel, err)
	}
	mode := filemode.Regular
	if info.Mode().Perm()&0o100 != 0 {
		mode = filemode.Executable
	}
	return data, mode, nil
}

// listWorkingFiles walks the project root and returns the sorted set of
// project-relative file paths, excluding .mgit/ and .git/. Directories are not
// included. Symlinks are NOT followed by filepath.WalkDir, so a symlink entry
// is reported as a non-directory file path and tracked as a symlink (its link
// text), never traversed into. Refs: MGIT-14.7 (#2)
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

// blobHashOfWorkingFile returns the git blob hash a working file's content
// would have, WITHOUT storing it. Used by Status to detect changes. It hashes
// the SAME bytes that would be committed (link text for a symlink, raw content
// otherwise) so a symlink does not perpetually appear modified. Refs: MGIT-14.7
func (r *Repository) blobHashOfWorkingFile(rel string) (plumbing.Hash, error) {
	data, _, err := r.workingFileContent(rel)
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

// writeEntryToDisk materializes a single tree entry onto the working file at
// the given project-relative path under the project root. See writeEntryToDir
// for the materialization semantics. Refs: MGIT-14.7 (#2, #3)
func (r *Repository) writeEntryToDisk(rel string, entry blobEntry) error {
	return r.writeEntryToDir(r.root, rel, entry)
}

// writeEntryToDir materializes a single tree entry (blob + git mode) onto the
// file at the given relative path UNDER root, creating parent directories as
// needed. root is the destination FS root: the project root for an in-place
// checkout, or a linked worktree's path for `mgit worktree add` (MGIT-17). A
// symlink entry (120000) is recreated as an actual symlink whose target is the
// blob's link text; a regular/executable entry is written as a file with the
// matching permission bits (so the executable bit survives a round-trip). Any
// pre-existing file at the path is replaced. rel is validated so it can never
// escape root. Refs: MGIT-14.7 (#2, #3), MGIT-17
func (r *Repository) writeEntryToDir(root, rel string, entry blobEntry) error {
	if err := validateRelPath(rel); err != nil {
		return err
	}
	data, err := r.blobContent(entry.hash)
	if err != nil {
		return fmt.Errorf("materialize %s: %w", rel, err)
	}
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o750); err != nil {
		return fmt.Errorf("mkdir for %s: %w", rel, err)
	}
	// Remove any existing path first so we can switch a regular file ↔ symlink
	// cleanly and apply fresh permissions.
	if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("replace working file %s: %w", rel, err)
	}
	if entry.mode == filemode.Symlink {
		if err := os.Symlink(string(data), abs); err != nil {
			return fmt.Errorf("write symlink %s: %w", rel, err)
		}
		return nil
	}
	perm := os.FileMode(0o600)
	if entry.mode == filemode.Executable {
		perm = 0o700
	}
	if err := os.WriteFile(abs, data, perm); err != nil {
		return fmt.Errorf("write working file %s: %w", rel, err)
	}
	return nil
}

// blobContent loads and returns the raw content of the blob at blobHash from
// the .mgit object store. Callers add path context to any returned error.
func (r *Repository) blobContent(blobHash plumbing.Hash) ([]byte, error) {
	blob, err := r.repo.BlobObject(blobHash)
	if err != nil {
		return nil, fmt.Errorf("load blob %s: %w", blobHash, err)
	}
	reader, err := blob.Reader()
	if err != nil {
		return nil, fmt.Errorf("read blob %s: %w", blobHash, err)
	}
	defer reader.Close() //nolint:errcheck // best-effort close after copy
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("read blob %s: %w", blobHash, err)
	}
	return data, nil
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
