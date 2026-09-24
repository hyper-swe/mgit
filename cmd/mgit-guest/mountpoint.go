package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// mountPointOps are the filesystem operations makeMountPoint needs, injected
// so the repair logic is testable off Linux and without a guest.
type mountPointOps struct {
	mkdirAll        func(path string, perm os.FileMode) error
	stat            func(path string) (os.FileInfo, error)
	isCopyUpRefusal func(err error) bool
	// shadow makes dir writable by mounting a tmpfs over it, seeded with
	// dir's own contents (ensureWritableDir).
	shadow func(dir string) error
}

// makeMountPoint creates the worktree's identical-path mount point in the
// guest root.
//
// On a guest root that cannot copy up — Linux libkrun, whose virtio-fs
// refuses FS_IOC_GETFLAGS with EOPNOTSUPP (MGIT-89) — a path beneath a
// directory the image ships cannot be created: writing into that directory
// needs it copied up first, and the lower filesystem refuses. A user's
// worktree lives exactly there (/home/<user>/…), so without a repair every
// guest launched for it died at boot. The repair is the one /etc already
// gets: shadow the DEEPEST directory that already exists with a tmpfs seeded
// from its own contents, then create the rest. Deepest, so the least of the
// image moves into memory; and only on the copy-up refusal, so a root that
// copies up (firecracker, vzf, macOS libkrun) is never touched.
// Refs: MGIT-230.7, MGIT-89
func makeMountPoint(path string, ops mountPointOps) error {
	err := ops.mkdirAll(path, 0o755)
	if err == nil {
		return nil
	}
	if !ops.isCopyUpRefusal(err) {
		return fmt.Errorf("mgit-guest: create worktree mount point %q: %w", path, err)
	}
	anc := deepestExistingAncestor(path, ops.stat)
	if anc == "/" {
		return fmt.Errorf("mgit-guest: create worktree mount point %q: %w; the only existing "+
			"ancestor is the root, which is never shadowed (a tmpfs over / would hide the image)", path, err)
	}
	if err := ops.shadow(anc); err != nil {
		return fmt.Errorf("mgit-guest: create worktree mount point %q: the guest root cannot copy up "+
			"(MGIT-89), and making %s writable failed: %w", path, anc, err)
	}
	if err := ops.mkdirAll(path, 0o755); err != nil {
		return fmt.Errorf("mgit-guest: create worktree mount point %q: still refused after %s "+
			"was shadowed with a writable tmpfs: %w", path, anc, err)
	}
	return nil
}

// deepestExistingAncestor returns the deepest proper ancestor of path that
// exists as a directory, or "/" when none below the root does.
func deepestExistingAncestor(path string, stat func(string) (os.FileInfo, error)) string {
	for dir := filepath.Dir(filepath.Clean(path)); dir != "/" && dir != "."; dir = filepath.Dir(dir) {
		if info, err := stat(dir); err == nil && info.IsDir() {
			return dir
		}
	}
	return "/"
}
