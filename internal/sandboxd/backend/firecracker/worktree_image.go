package firecracker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// worktreeImageName is the per-VM worktree filesystem image, kept in the
// sandbox state dir so teardown (one RemoveAll) leaves no host residue.
const worktreeImageName = "worktree.ext4"

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

// buildWorktreeImage packs a worktree's working-tree files into a writable
// ext4 image at imagePath, for firecracker's copy-and-land worktree
// delivery (ADR-005, no virtiofs): the guest mounts this image at the
// worktree's identical path and edits/builds against the copy; only
// committed+verified objects return via land. The image is pre-sized as a
// sparse file so mke2fs fills it deterministically; on a build failure the
// partial image is removed. The shared .mgit store is a sibling of the
// worktree, never inside it, so packing the worktree subtree cannot leak
// it (SEC-03). Refs: FR-17.3, MGIT-11.6.4
func buildWorktreeImage(ctx context.Context, runner mkfsRunner, worktreePath, imagePath string, sizeMB int) error {
	if !filepath.IsAbs(worktreePath) {
		return fmt.Errorf("firecracker worktree image: worktree path must be absolute, got %q", worktreePath)
	}
	if fi, err := os.Stat(worktreePath); err != nil || !fi.IsDir() {
		return fmt.Errorf("firecracker worktree image: worktree %q is not a directory: %w", worktreePath, err)
	}
	if sizeMB <= 0 {
		sizeMB = defaultWorktreeImageMB
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
	if err := runner.run(ctx, mke2fsArgs(worktreePath, imagePath)...); err != nil {
		_ = os.Remove(imagePath)
		return fmt.Errorf("firecracker worktree image: mke2fs: %w", err)
	}
	return nil
}

// mke2fsArgs builds the rootless mke2fs invocation that creates an ext4
// filesystem in the pre-sized image and populates it from the worktree
// directory (-d). No root is required for -d. Refs: MGIT-11.6.4
func mke2fsArgs(worktreePath, imagePath string) []string {
	return []string{"-F", "-q", "-t", "ext4", "-d", worktreePath, imagePath}
}
