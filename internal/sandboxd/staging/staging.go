// Package staging builds the SEC-03 quarantined delivery tree every sandbox
// backend hands to its guest. It is the ONE security-critical implementation
// of the worktree-to-guest copy-and-land contract, shared by the firecracker
// (ext4 image), vzf (virtiofs share), and container (bind-mount) backends so
// the guarantee is defined in a single place rather than re-derived per
// backend.
//
// The guest is the hostile party. Build assembles a staging tree that contains
// ONLY the worktree's working-tree files plus a private, sandbox-local mgit
// object store laid in at <worktree>/.mgit, and it enforces the delivery
// invariants host-side, before the guest can act:
//
//   - any in-worktree store directory (.mgit/.git) at the worktree root is
//     dropped, so a packed clone's history never reaches the guest (finding
//     F-A) — the guest's only store is the private one laid in;
//   - any symlink whose resolved target ESCAPES the worktree is rejected
//     (ErrSymlinkEscape, finding F-A/NEW-2): a guest that could follow such a
//     link would reach host paths outside its quarantine.
//
// It fails closed: a single escaping symlink aborts the whole build, so a
// backend that propagates the error never delivers an unquarantined tree.
// Pure Go, host-only, OS-neutral.
// Refs: SEC-03, FR-17.3, FR-17.5, F-A/NEW-2
package staging

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ErrSymlinkEscape is returned when a worktree symlink resolves to a target
// outside the worktree on delivery — a containment escape the host rejects
// before the guest can follow it (finding F-A/NEW-2). Backends errors.Is it
// to reject the launch as a quarantine failure. Refs: SEC-03, F-A/NEW-2
var ErrSymlinkEscape = errors.New("worktree symlink target escapes the worktree")

// GuestStoreName is the worktree-relative directory the private store is
// delivered at inside the guest — the .mgit the guest's mgit commits into and
// ServeLandHead serves. Mirrors quarantine.guestStoreName and the ADR-001
// amendment (MGIT-14): mgit's store is .mgit, never a .git at the worktree
// root. Refs: SEC-03, MGIT-14
const GuestStoreName = ".mgit"

// Build copies the worktree into stagingDir for delivery, enforcing the SEC-03
// invariants, then lays the private store in at <stagingDir>/.mgit. stagingDir
// is created (0700) if absent; it MUST be outside the worktree (it is
// sandbox-local host state, cleaned by teardown). The result is a tree a
// backend can pack/share/mount verbatim as the guest's worktree.
//
// It fails closed: a single escaping symlink aborts the whole build (the
// partial staging tree is the caller's to remove). Refs: SEC-03, F-A/NEW-2
func Build(worktreePath, privateStorePath, stagingDir string) error {
	if err := os.MkdirAll(stagingDir, 0o700); err != nil {
		return fmt.Errorf("create staging dir: %w", err)
	}
	root := filepath.Clean(worktreePath)
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		// Drop any in-worktree git/mgit store DIRECTORY at any depth: the guest's
		// only store is the private one laid in below. A packed store would
		// defeat the rebind — at the root (a clone of the repo) OR nested (a
		// vendored/submodule clone like vendor/foo/.git), whose history would
		// otherwise reach the guest (finding F1, the deep form of F-A).
		if d.IsDir() && isStoreDir(rel) {
			return filepath.SkipDir
		}
		dst := filepath.Join(stagingDir, rel)
		return copyStagingEntry(root, path, rel, dst, d)
	})
	if err != nil {
		return err
	}
	// Lay the private store in at <staging>/.mgit (the guest's only store).
	return copyTree(privateStorePath, filepath.Join(stagingDir, GuestStoreName))
}

// isStoreDir reports whether a worktree-relative path's base name is a git/mgit
// store directory (.mgit or .git) that must not be packed — at ANY depth, so a
// nested/vendored clone's store never reaches the guest, not just the root one.
func isStoreDir(rel string) bool {
	base := filepath.Base(rel)
	return base == GuestStoreName || base == ".git"
}

// copyStagingEntry copies one walked worktree entry into the staging tree,
// rejecting any symlink whose resolved target escapes the worktree (SEC-03).
func copyStagingEntry(root, path, rel, dst string, d os.DirEntry) error {
	switch {
	case d.IsDir():
		return os.MkdirAll(dst, 0o750)
	case d.Type()&os.ModeSymlink != 0:
		if err := assertSymlinkWithin(root, path); err != nil {
			return err
		}
		target, err := os.Readlink(path)
		if err != nil {
			return fmt.Errorf("read symlink %s: %w", rel, err)
		}
		return os.Symlink(target, dst)
	default:
		return copyFile(path, dst)
	}
}

// assertSymlinkWithin rejects a symlink whose RESOLVED target escapes the
// worktree root. The target is resolved relative to the link's directory and
// fully evaluated (EvalSymlinks where it exists) so a chain of links cannot
// step outside. A link to a not-yet-existing in-worktree path is allowed; one
// pointing outside the worktree (absolute or via ..) is rejected. Refs: SEC-03, F-A/NEW-2
func assertSymlinkWithin(root, linkPath string) error {
	target, err := os.Readlink(linkPath)
	if err != nil {
		return fmt.Errorf("read symlink %s: %w", linkPath, err)
	}
	resolved := target
	if !filepath.IsAbs(resolved) {
		resolved = filepath.Join(filepath.Dir(linkPath), target)
	}
	resolved = filepath.Clean(resolved)
	// Fully evaluate where possible (collapses any intermediate links); fall
	// back to the lexical clean for a target that does not yet exist.
	if eval, evalErr := filepath.EvalSymlinks(resolved); evalErr == nil {
		resolved = eval
	}
	// Evaluate the root too so the containment check compares like with like:
	// on some platforms the worktree root itself sits under a symlinked prefix
	// (e.g. macOS /var -> /private/var), which EvalSymlinks expands on the
	// target but not on a lexically-cleaned root.
	cleanRoot := filepath.Clean(root)
	if eval, evalErr := filepath.EvalSymlinks(cleanRoot); evalErr == nil {
		cleanRoot = eval
	}
	if resolved == cleanRoot {
		return nil
	}
	rel, err := filepath.Rel(cleanRoot, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("%w: symlink %s -> %s escapes the worktree", ErrSymlinkEscape, linkPath, target)
	}
	return nil
}

// copyTree recursively copies src into dst (files, dirs, symlinks verbatim).
// Used to lay the private store into the staging tree; the private store is
// host-trusted (the host built it), so its symlinks (if any) are copied as-is.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		out := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(out, 0o750)
		case d.Type()&os.ModeSymlink != 0:
			target, lerr := os.Readlink(path)
			if lerr != nil {
				return fmt.Errorf("read symlink %s: %w", rel, lerr)
			}
			//nolint:gosec // G122: src is the HOST-BUILT private store (provision.Provisioner), not guest-controlled input; no TOCTOU surface from a hostile party. Worktree symlinks (the hostile source) go through copyStagingEntry, which rejects escapes first.
			return os.Symlink(target, out)
		default:
			return copyFile(path, out)
		}
	})
}

// copyFile copies a regular file from src to dst, preserving its mode.
func copyFile(src, dst string) error {
	fi, err := os.Lstat(src)
	if err != nil {
		return fmt.Errorf("stat %s: %w", src, err)
	}
	in, err := os.Open(src) //nolint:gosec // path from a manager-owned worktree/staging walk
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer func() { _ = in.Close() }()
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return fmt.Errorf("mkdir for %s: %w", dst, err)
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, fi.Mode().Perm()) //nolint:gosec // manager-owned staging dir
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return fmt.Errorf("copy %s: %w", dst, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close %s: %w", dst, err)
	}
	return nil
}
