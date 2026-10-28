//go:build linux

package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// overlayScratchDir is where the upper and work directories of a mount-point
// overlay live. It is /tmp, a tmpfs by the time the worktree is mounted (the
// seeded copy stages there too), so it is memory and takes no copy-up. /mnt is
// NOT usable: measured on Linux libkrun, creating a directory there is refused
// with EOPNOTSUPP like any other path the image ships. Refs: MGIT-230.8
const overlayScratchDir = "/tmp"

// overlayOps are the operations overlayShadow performs, injected so the
// choice between the overlay and the seeded copy is testable without a guest.
type overlayOps struct {
	mkdirAll func(path string, perm os.FileMode) error
	mount    func(source, target, fstype string, flags uintptr, data string) error
	// copyShadow is the repair used when the overlay is refused: shadow dir
	// with a tmpfs seeded with its contents (ensureWritableDir).
	copyShadow func(dir string) error
	logger     *slog.Logger
}

// overlayShadow makes dir writable for the one thing the worktree needs, a new
// directory beneath it, without copying anything.
//
// A new overlay is mounted AT dir with dir itself as its lower layer and a
// fresh upper and work directory in memory. dir's contents stay visible through
// the lower layer; a path created beneath dir lands in the new upper, whose
// root always exists, so no copy-up of dir is needed and none can be refused.
// The seeded copy this replaces moved the whole directory into guest memory,
// twice: harmless for /home, the image's entire /var or /opt otherwise.
//
// If the kernel refuses the overlay, the repair falls back to the seeded copy
// and says so, rather than leaving the guest unbootable. Refs: MGIT-230.8
func overlayShadow(dir string, ops overlayOps) error {
	base := filepath.Join(overlayScratchDir, ".mgit-mp-"+strings.ReplaceAll(strings.Trim(dir, "/"), "/", "_"))
	upper, work := filepath.Join(base, "upper"), filepath.Join(base, "work")
	for _, d := range []string{upper, work} {
		if err := ops.mkdirAll(d, 0o755); err != nil {
			return ops.fallBack(dir, fmt.Errorf("create %s: %w", d, err))
		}
	}
	opts := "lowerdir=" + dir + ",upperdir=" + upper + ",workdir=" + work
	if err := ops.mount("overlay", dir, "overlay", 0, opts); err != nil {
		return ops.fallBack(dir, fmt.Errorf("mount overlay at %s: %w", dir, err))
	}
	return nil
}

func (ops overlayOps) fallBack(dir string, cause error) error {
	if ops.logger != nil {
		ops.logger.Warn("mount-point overlay refused; shadowing the directory with a seeded tmpfs instead",
			"event", "mount_point_overlay_refused", "dir", dir, "cause", cause.Error())
	}
	return ops.copyShadow(dir)
}

// shadowMountPointDir is the shadow step makeMountPoint uses on a guest root
// that cannot copy up.
func shadowMountPointDir(dir string, logger *slog.Logger) error {
	return overlayShadow(dir, overlayOps{
		mkdirAll:   os.MkdirAll,
		mount:      unix.Mount,
		copyShadow: func(d string) error { return ensureWritableDir(d, logger) },
		logger:     logger,
	})
}
