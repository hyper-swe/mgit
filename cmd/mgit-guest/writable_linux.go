//go:build linux

package main

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// The writable-root overlay does not always deliver a writable root, and this
// file is what makes the guest usable when it does not.
//
// WHY THIS EXISTS, measured on real KVM (MGIT-89). mgit-guest overlays a tmpfs
// upper on the read-only image root, which is meant to make the whole root
// writable. On Linux/libkrun it does not: writing anything that lives in the
// LOWER — `/etc/resolv.conf`, or a `chmod` of `/etc` on its own — fails with
// EOPNOTSUPP, while writes to paths that exist only in the upper succeed. The
// upper is fine (tmpfs, and it takes `trusted.*` xattrs); the lower is fine to
// read. What fails is overlayfs's COPY-UP, and the cause is one errno:
//
//	overlayfs calls ovl_copy_fileattr() on every regular-file and directory
//	copy-up, which issues FS_IOC_GETFLAGS on the lower inode. It tolerates
//	ENOTTY and EINVAL as "this filesystem has no file attributes" and
//	PROPAGATES anything else. libkrun's Linux virtio-fs answers EOPNOTSUPP,
//	so every copy-up fails; libkrun's macOS filesystem device answers ENOTTY,
//	so every copy-up succeeds. Same guest kernel (libkrunfw 6.12.91), same
//	overlay options — no overlay option set changes it (userxattr, index=off,
//	metacopy=off, redirect_dir=off, xino=off were all measured).
//
// That is an upstream issue in libkrun's filesystem server, not something mgit
// can fix in the kernel or in the overlay. What mgit CAN do is stop depending
// on copy-up for the one directory the guest must write to function at all:
// /etc, where the resolver lives. Without it mgit-guest dies during network
// setup and NO networked sandbox starts on this backend.
//
// It is a CAPABILITY PROBE, not a platform check, and that is deliberate: the
// repair runs only where a write actually fails, so firecracker, vzf and
// macOS/libkrun are untouched, and the day libkrun's virtio-fs answers ENOTTY
// the probe passes and this code stops running on its own. Refs: MGIT-89,
// FR-17.17, MGIT-68

// seedStagingDir is where a shadowed directory's contents are parked while the
// tmpfs is mounted over it. It lives under /tmp, which is already a tmpfs by
// the time this runs.
const seedStagingDir = "/tmp/.mgit-seed"

// ensureWritableDir guarantees the guest can write inside dir.
//
// When dir is already writable it does nothing, which is the case on every
// backend whose overlay can copy up. When it is not, the directory's contents
// are snapshotted, a tmpfs is mounted over it, and the snapshot is restored —
// so the guest gets a writable /etc that still carries the base image's
// resolver config, CA bundle, passwd and nsswitch rather than an empty one.
//
// A failure to REPAIR is fatal to the boot, deliberately: a guest that comes up
// with an unwritable /etc fails its network setup a moment later, and failing
// there gives an error that names the network instead of the cause.
// Refs: MGIT-89
func ensureWritableDir(dir string, logger *slog.Logger) error {
	// A base that ships no such directory is not a failure: create it. It lands
	// in the overlay's upper, where writes work even on a root that cannot copy
	// up — which is exactly why this must come before the probe's verdict.
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
			return fmt.Errorf("mgit-guest: create %s: %w", dir, mkErr)
		}
	}
	if err := dirWritable(dir); err == nil {
		return nil
	} else if !errors.Is(err, unix.EOPNOTSUPP) {
		// Anything other than the copy-up refusal is not this workaround's
		// business — report it rather than papering over it with a tmpfs.
		return fmt.Errorf("mgit-guest: %s is not writable: %w", dir, err)
	}
	logger.Warn("guest root cannot copy up; shadowing directory with a tmpfs",
		"event", "overlay_copyup_unsupported", "dir", dir,
		"detail", "the image root's filesystem refuses FS_IOC_GETFLAGS (EOPNOTSUPP), "+
			"so overlayfs cannot copy up; see MGIT-89")

	staging := filepath.Join(seedStagingDir, filepath.Base(dir))
	if err := copyTree(dir, staging); err != nil {
		return err
	}
	if err := unix.Mount("tmpfs", dir, "tmpfs", 0, ""); err != nil {
		return fmt.Errorf("mgit-guest: mount tmpfs over %s: %w", dir, err)
	}
	if err := copyTree(staging, dir); err != nil {
		return err
	}
	if err := os.RemoveAll(seedStagingDir); err != nil {
		return fmt.Errorf("mgit-guest: clear %s: %w", seedStagingDir, err)
	}
	if err := dirWritable(dir); err != nil {
		return fmt.Errorf("mgit-guest: %s still not writable after seeding: %w", dir, err)
	}
	return nil
}

// dirWritable reports whether a file can be created in dir, by creating one.
// Probing by ACTION rather than by mode bits is the point: the failure this
// guards against is a filesystem refusing the operation, which no permission
// check would reveal. Refs: MGIT-89
func dirWritable(dir string) error {
	probe := filepath.Join(dir, ".mgit-write-probe")
	f, err := os.OpenFile(probe, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_ = f.Close()
	return os.Remove(probe)
}

// copyTree copies a directory tree, preserving the entry kinds and modes that
// a guest base's /etc actually contains: directories, regular files and
// symlinks. Ownership is preserved where the caller can set it (mgit-guest is
// PID 1, so it can).
//
// Anything else — sockets, devices, fifos — is SKIPPED rather than guessed at:
// /etc holds none in a normal base, and silently inventing a node of the wrong
// type would be worse than its absence. Refs: MGIT-89
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("mgit-guest: walk %s: %w", path, err)
		}
		rel, rerr := filepath.Rel(src, path)
		if rerr != nil {
			return fmt.Errorf("mgit-guest: relativize %s: %w", path, rerr)
		}
		target := filepath.Join(dst, rel)
		info, ierr := d.Info()
		if ierr != nil {
			return fmt.Errorf("mgit-guest: stat %s: %w", path, ierr)
		}
		switch {
		case d.IsDir():
			return copyDir(target, info)
		case info.Mode()&os.ModeSymlink != 0:
			return copySymlink(path, target)
		case info.Mode().IsRegular():
			return copyRegular(path, target, info)
		default:
			return nil // sockets/devices/fifos: see the doc comment
		}
	})
}

// copyDir recreates a directory with its source mode and ownership.
func copyDir(target string, info os.FileInfo) error {
	if err := os.MkdirAll(target, info.Mode().Perm()); err != nil {
		return fmt.Errorf("mgit-guest: mkdir %s: %w", target, err)
	}
	if err := os.Chmod(target, info.Mode().Perm()); err != nil {
		return fmt.Errorf("mgit-guest: chmod %s: %w", target, err)
	}
	return chownLike(target, info)
}

// copySymlink recreates a symlink, link text unchanged (it may dangle, which
// is normal in a base image's /etc and must be preserved as-is).
func copySymlink(path, target string) error {
	link, err := os.Readlink(path)
	if err != nil {
		return fmt.Errorf("mgit-guest: readlink %s: %w", path, err)
	}
	if err := os.Symlink(link, target); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("mgit-guest: symlink %s: %w", target, err)
	}
	return nil
}

// copyRegular copies a regular file's bytes, mode and ownership.
func copyRegular(path, target string, info os.FileInfo) error {
	in, err := os.Open(path) //nolint:gosec // guest-local base image path
	if err != nil {
		return fmt.Errorf("mgit-guest: open %s: %w", path, err)
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm()) //nolint:gosec // guest-local
	if err != nil {
		return fmt.Errorf("mgit-guest: create %s: %w", target, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return fmt.Errorf("mgit-guest: copy %s: %w", target, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("mgit-guest: close %s: %w", target, err)
	}
	if err := os.Chmod(target, info.Mode().Perm()); err != nil {
		return fmt.Errorf("mgit-guest: chmod %s: %w", target, err)
	}
	return chownLike(target, info)
}

// chownLike gives target the source's uid/gid when the platform reports them.
// A failure is tolerated: the seeded copy is more useful with the wrong owner
// than absent, and mgit-guest runs everything as root inside the guest anyway.
func chownLike(target string, info os.FileInfo) error {
	st, ok := info.Sys().(*unix.Stat_t)
	if !ok {
		return nil
	}
	_ = os.Lchown(target, int(st.Uid), int(st.Gid))
	return nil
}
