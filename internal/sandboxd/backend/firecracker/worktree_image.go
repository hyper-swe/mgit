package firecracker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// errSymlinkEscape is returned when a worktree symlink resolves to a target
// outside the worktree on delivery — a containment escape the host rejects
// before the guest can follow it (finding F-A/NEW-2). Refs: SEC-03
var errSymlinkEscape = errors.New("worktree symlink target escapes the worktree")

// worktreeImageName is the per-VM worktree filesystem image, kept in the
// sandbox state dir so teardown (one RemoveAll) leaves no host residue.
const worktreeImageName = "worktree.ext4"

// stagingDirName is the per-VM staging tree mke2fs packs the image from: the
// worktree's working-tree files plus the SEC-03 private .mgit store, with any
// in-worktree store and escaping symlink removed. It lives in the sandbox
// state dir (teardown's RemoveAll clears it). Refs: SEC-03
const stagingDirName = "worktree-staging"

// guestStoreName is the worktree-relative directory the private store is
// delivered at inside the guest image — the .mgit the guest's mgit commits
// into and ServeLandHead serves. Mirrors quarantine.guestStoreName. Refs: SEC-03
const guestStoreName = ".mgit"

// defaultWorktreeImageMB sizes the worktree image when the caller leaves
// it unset; it must hold the worktree's files plus the guest's build
// output. Mirrors the COW overlay default (NFR-17.5).
const defaultWorktreeImageMB = 4096

// mkfsRunner builds an ext4 filesystem image. The real implementation
// execs mke2fs (linux); tests inject a fake. It is the only OS-specific
// part of worktree delivery, so the builder logic stays portable and
// unit-testable without mke2fs or KVM.
type mkfsRunner interface {
	run(ctx context.Context, args ...string) error
}

// imageFile is the pre-sized image handle buildWorktreeImage sizes before
// mke2fs fills it. An interface (not *os.File) so a test can inject a
// handle whose Truncate/Close fail, exercising the fail-closed cleanup.
type imageFile interface {
	Truncate(size int64) error
	Close() error
}

// createImageFile opens the image as a new exclusive file. A var so tests
// can fault-inject the sizing/close failure paths; never reassigned in
// production.
var createImageFile = func(path string) (imageFile, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // manager-owned state dir
}

// buildWorktreeImage packs a worktree's working-tree files plus the SEC-03
// private object store into a writable ext4 image at imagePath, for
// firecracker's copy-and-land worktree delivery (ADR-005, no virtiofs): the
// guest mounts this image at the worktree's identical path, commits into the
// private .mgit inside it, and only committed+verified objects return via
// land. It is the part that makes SEC-03 REAL on the proven KVM backend:
//
//   - A staging tree is assembled from the worktree with escaping symlinks
//     REJECTED (a symlink whose resolved target leaves the worktree is a
//     containment escape — finding F-A/NEW-2) and any IN-worktree store
//     (.mgit/.git) dropped, so nothing but working-tree files is packed.
//   - The private store (privateStorePath) is laid in at <worktree>/.mgit, so
//     the guest commits into the sandbox-local store, never the host shared one
//     (which is a sibling of the worktree and so was never packable anyway).
//
// The image is pre-sized as a sparse file so mke2fs fills it deterministically;
// on any failure the partial image and staging tree are removed (fail closed).
// When privateStorePath is empty (no provisioner wired) the worktree is packed
// directly, the pre-SEC-03 behavior. Refs: FR-17.3, SEC-03, MGIT-11.6.4, MGIT-11.6.8
func buildWorktreeImage(ctx context.Context, runner mkfsRunner, worktreePath, imagePath, privateStorePath string, sizeMB int) error {
	if !filepath.IsAbs(worktreePath) {
		return fmt.Errorf("firecracker worktree image: worktree path must be absolute, got %q", worktreePath)
	}
	if fi, err := os.Stat(worktreePath); err != nil || !fi.IsDir() {
		return fmt.Errorf("firecracker worktree image: worktree %q is not a directory: %w", worktreePath, err)
	}
	if sizeMB <= 0 {
		sizeMB = defaultWorktreeImageMB
	}

	// Assemble the staging tree the image is built from. With no private store
	// (legacy/direct path) the worktree is packed directly; otherwise a
	// quarantined staging tree (escaping symlinks rejected, private store laid
	// in) is built and packed. stagingDir == "" means "pack the worktree".
	srcDir := worktreePath
	stagingDir := ""
	if privateStorePath != "" {
		stagingDir = filepath.Join(filepath.Dir(imagePath), stagingDirName)
		if err := buildStagingTree(worktreePath, privateStorePath, stagingDir); err != nil {
			_ = os.RemoveAll(stagingDir)
			return fmt.Errorf("firecracker worktree image: stage: %w", err)
		}
		srcDir = stagingDir
		defer func() { _ = os.RemoveAll(stagingDir) }()
	}

	// Pre-size the image as a sparse file so mke2fs uses an explicit size
	// (no reliance on mke2fs's create-if-missing behavior).
	f, err := createImageFile(imagePath)
	if err != nil {
		return fmt.Errorf("firecracker worktree image: create %q: %w", imagePath, err)
	}
	if err := f.Truncate(int64(sizeMB) << 20); err != nil {
		_ = f.Close()
		_ = os.Remove(imagePath)
		return fmt.Errorf("firecracker worktree image: size %q: %w", imagePath, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(imagePath)
		return fmt.Errorf("firecracker worktree image: close %q: %w", imagePath, err)
	}
	if err := runner.run(ctx, mke2fsArgs(srcDir, imagePath)...); err != nil {
		_ = os.Remove(imagePath)
		return fmt.Errorf("firecracker worktree image: mke2fs: %w", err)
	}
	return nil
}

// buildStagingTree copies the worktree into stagingDir for packing, enforcing
// the SEC-03 delivery invariants, then lays the private store in at
// <staging>/.mgit:
//
//   - Any symlink whose resolved target ESCAPES the worktree is rejected
//     (finding F-A/NEW-2): a guest that could follow such a link would reach
//     host paths outside its quarantine. In-worktree symlinks are preserved.
//   - Any in-worktree store directory (.mgit/.git) at the root is skipped — the
//     guest's only store is the private one laid in below; a packed in-worktree
//     store could expose a clone's history (the original F-A concern).
//
// It fails closed: a single escaping symlink aborts the whole build. Refs: SEC-03
func buildStagingTree(worktreePath, privateStorePath, stagingDir string) error {
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
		// Drop any in-worktree store at the root: the guest's store is the
		// private one; a packed .mgit/.git would defeat the rebind.
		if isRootStoreDir(rel) {
			return filepath.SkipDir
		}
		dst := filepath.Join(stagingDir, rel)
		return copyStagingEntry(root, path, rel, dst, d)
	})
	if err != nil {
		return err
	}
	// Lay the private store in at <staging>/.mgit (the guest's only store).
	return copyTree(privateStorePath, filepath.Join(stagingDir, guestStoreName))
}

// isRootStoreDir reports whether a worktree-relative path is a store directory
// at the worktree root (.mgit or .git) that must not be packed.
func isRootStoreDir(rel string) bool {
	return rel == guestStoreName || rel == ".git"
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
		return fmt.Errorf("%w: symlink %s -> %s escapes the worktree", errSymlinkEscape, linkPath, target)
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

// mke2fsArgs builds the rootless mke2fs invocation that creates an ext4
// filesystem in the pre-sized image and populates it from srcDir (-d). No
// root is required for -d. Refs: MGIT-11.6.4
func mke2fsArgs(srcDir, imagePath string) []string {
	return []string{"-F", "-q", "-t", "ext4", "-d", srcDir, imagePath}
}
