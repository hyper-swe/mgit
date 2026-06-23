package firecracker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/hyper-swe/mgit/internal/sandboxd/staging"
)

// errSymlinkEscape re-exports the shared staging sentinel so this package's
// tests and the hostile-guest e2e keep matching on the firecracker name while
// the one security-critical implementation lives in internal/sandboxd/staging.
// Refs: SEC-03, F-A/NEW-2
var errSymlinkEscape = staging.ErrSymlinkEscape

// worktreeImageName is the per-VM worktree filesystem image, kept in the
// sandbox state dir so teardown (one RemoveAll) leaves no host residue.
const worktreeImageName = "worktree.ext4"

// stagingDirName is the per-VM staging tree mke2fs packs the image from: the
// worktree's working-tree files plus the SEC-03 private .mgit store, with any
// in-worktree store and escaping symlink removed. It lives in the sandbox
// state dir (teardown's RemoveAll clears it). Refs: SEC-03
const stagingDirName = "worktree-staging"

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
// land. It is the part that makes SEC-03 REAL on the proven KVM backend: the
// staging tree (built by the shared staging package — escaping symlinks
// REJECTED, any in-worktree store dropped, the private store laid in at
// <worktree>/.mgit) is the only thing packed, so the guest's only store is the
// sandbox-local one and the host shared store (a sibling of the worktree) is
// never packable.
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
	// (legacy/direct path) the worktree is packed directly; otherwise the shared
	// staging builder produces a quarantined tree (escaping symlinks rejected,
	// private store laid in). stagingDir == "" means "pack the worktree".
	srcDir := worktreePath
	stagingDir := ""
	if privateStorePath != "" {
		stagingDir = filepath.Join(filepath.Dir(imagePath), stagingDirName)
		if err := staging.Build(worktreePath, privateStorePath, stagingDir); err != nil {
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

// mke2fsArgs builds the rootless mke2fs invocation that creates an ext4
// filesystem in the pre-sized image and populates it from srcDir (-d). No
// root is required for -d. Refs: MGIT-11.6.4
func mke2fsArgs(srcDir, imagePath string) []string {
	return []string{"-F", "-q", "-t", "ext4", "-d", srcDir, imagePath}
}
