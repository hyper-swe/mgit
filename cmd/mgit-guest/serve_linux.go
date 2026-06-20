//go:build linux

package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"

	"github.com/mdlayher/vsock"
	"golang.org/x/sys/unix"

	"github.com/hyper-swe/mgit/internal/guest"
	"github.com/hyper-swe/mgit/internal/guestboot"
)

// procCmdline is the kernel command line the host appends the worktree
// descriptor to; a var so tests can supply a fixture path.
var procCmdline = "/proc/cmdline"

// serveGuest performs PID-1 duties — mount the worktree at its identical
// host path and a tmpfs /tmp — then accepts exec connections over vsock,
// serving each with the supervisor. One connection is one exec request
// (FR-17.11).
func serveGuest(ctx context.Context, supervisor *guest.Supervisor, port uint32, logger *slog.Logger) error {
	if err := mountGuestFilesystems(); err != nil {
		return err
	}

	listener, err := vsock.Listen(port, nil)
	if err != nil {
		return fmt.Errorf("mgit-guest: vsock listen :%d: %w", port, err)
	}
	defer func() { _ = listener.Close() }()

	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()

	logger.Info("mgit-guest serving", "event", "started", "vsock_port", port)
	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
				return fmt.Errorf("mgit-guest: accept: %w", err)
			}
		}
		go serveConn(ctx, supervisor, conn, logger)
	}
}

// serveConn serves one exec connection and closes it.
func serveConn(ctx context.Context, supervisor *guest.Supervisor, conn net.Conn, logger *slog.Logger) {
	defer func() { _ = conn.Close() }()
	if err := supervisor.Serve(ctx, conn); err != nil {
		logger.Error("mgit-guest exec failed", "event", "exec_error", "error", err.Error())
	}
}

// mountGuestFilesystems mounts the worktree at its identical absolute host
// path (FR-17.3) and a tmpfs at /tmp. The worktree descriptor — path,
// filesystem type, and source — is supplied by the host on the kernel
// command line (guestboot), so the same guest serves both delivery
// mechanisms: a virtiofs tag share (vzf) or a virtio-blk ext4 device
// (firecracker). A partial/invalid descriptor fails closed; a wholly
// absent one mounts only /tmp (a no-worktree sandbox). Idempotent enough
// for boot: an already-mounted target is tolerated.
func mountGuestFilesystems() error {
	// mgit-guest is PID 1, so it mounts the kernel's pseudo-filesystems
	// itself. /proc first (the worktree descriptor is read from
	// /proc/cmdline); /dev (devtmpfs) before the worktree so a block-device
	// worktree (firecracker /dev/vdc) exists to mount. Then make the root
	// writable (overlay) BEFORE creating the worktree mount point or /tmp,
	// since the pinned image root is read-only (FR-17.17). The mount points
	// are created defensively for minimal rootfs images.
	if err := mountPseudoFS("proc", "/proc", "proc"); err != nil {
		return err
	}
	if err := mountPseudoFS("devtmpfs", "/dev", "devtmpfs"); err != nil {
		return err
	}
	if err := makeRootWritable(); err != nil {
		return err
	}
	if err := mountPseudoFS("tmpfs", "/tmp", "tmpfs"); err != nil {
		return err
	}
	if err := mountWorktree(); err != nil {
		return err
	}
	return nil
}

// overlayScratch is a baked empty dir used as a tmpfs scratch for the
// writable-root overlay (its upper + work layers and the new-root mount
// point). It must exist in the read-only image.
const overlayScratch = "/mnt"

// makeRootWritable gives the guest a writable root over the read-only
// pinned image (FR-17.17): it overlays a writable upper on the image root
// and switch_roots into the merged view, so mgit-guest can create the
// worktree's identical mount point — and the guest can write outside the
// worktree during a build — without mutating the immutable image. The
// image stays the overlay's read-only lower.
//
// v1 uses a TMPFS upper (RAM-backed, ephemeral) for simplicity and to
// avoid an in-guest mkfs. This deliberately differs from the cached
// image's /sbin/overlay-init, which uses the disk-backed COW overlay drive
// (vdb) the backend already attaches; moving the upper onto vdb (the
// FR-17.17/NFR-17.7 disk-backed COW) is a tracked follow-on (MGIT-11.6.7).
//
// It is intentionally NOT idempotent (unlike mountPseudoFS/mountWorktree,
// which tolerate EBUSY): switch_root must run exactly once at boot, so any
// error here is fatal rather than tolerated. Refs: FR-17.3, FR-17.17, MGIT-11.6.6
func makeRootWritable() error {
	// A tmpfs scratch holds the overlay upper + work dirs and the new-root
	// mount point (/ is still read-only here, so the scratch is a writable
	// mount over a baked empty dir).
	if err := unix.Mount("tmpfs", overlayScratch, "tmpfs", 0, ""); err != nil {
		return fmt.Errorf("mgit-guest: mount overlay scratch %s: %w", overlayScratch, err)
	}
	upper, work, newRoot := overlayScratch+"/upper", overlayScratch+"/work", overlayScratch+"/newroot"
	for _, d := range []string{upper, work, newRoot} {
		if err := os.Mkdir(d, 0o755); err != nil { //nolint:gosec // tmpfs scratch, not host-trusted
			return fmt.Errorf("mgit-guest: create overlay dir %s: %w", d, err)
		}
	}
	// Merge the read-only image root (lower) with the writable tmpfs (upper).
	opts := "lowerdir=/,upperdir=" + upper + ",workdir=" + work
	if err := unix.Mount("overlay", newRoot, "overlay", 0, opts); err != nil {
		return fmt.Errorf("mgit-guest: mount overlay root: %w", err)
	}
	// Carry the already-mounted pseudo-filesystems into the new root so they
	// survive switch_root (their mount points exist in the lower image).
	for _, m := range []string{"/proc", "/dev"} {
		if err := unix.Mount(m, newRoot+m, "", unix.MS_MOVE, ""); err != nil {
			return fmt.Errorf("mgit-guest: move %s into new root: %w", m, err)
		}
	}
	// switch_root into the writable overlay (util-linux style): make the
	// mount tree private (so MS_MOVE onto / is allowed), move newroot onto
	// /, chroot in. The read-only image remains the overlay's lower.
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("mgit-guest: make mount tree private: %w", err)
	}
	if err := unix.Chdir(newRoot); err != nil {
		return fmt.Errorf("mgit-guest: chdir new root: %w", err)
	}
	if err := unix.Mount(".", "/", "", unix.MS_MOVE, ""); err != nil {
		return fmt.Errorf("mgit-guest: move new root to /: %w", err)
	}
	if err := unix.Chroot("."); err != nil {
		return fmt.Errorf("mgit-guest: chroot new root: %w", err)
	}
	if err := unix.Chdir("/"); err != nil {
		return fmt.Errorf("mgit-guest: chdir /: %w", err)
	}
	return nil
}

// mountPseudoFS mounts a kernel pseudo-filesystem, creating the target
// first; an already-mounted target (EBUSY) is tolerated for boot idempotence.
func mountPseudoFS(source, target, fstype string) error {
	if err := os.MkdirAll(target, 0o555); err != nil { //nolint:gosec // guest pseudo-fs mount point, shadowed by the mount
		return fmt.Errorf("mgit-guest: create %s: %w", target, err)
	}
	if err := unix.Mount(source, target, fstype, 0, ""); err != nil && err != unix.EBUSY {
		return fmt.Errorf("mgit-guest: mount %s: %w", target, err)
	}
	return nil
}

// mountWorktree reads the host-supplied worktree descriptor from the
// kernel command line and mounts the worktree at its identical absolute
// path. Refs: FR-17.3, MGIT-11.6.5
func mountWorktree() error {
	cmdline, err := os.ReadFile(procCmdline)
	if err != nil {
		return fmt.Errorf("mgit-guest: read kernel cmdline: %w", err)
	}
	wt := guestboot.ParseWorktreeMount(string(cmdline))
	if wt.Empty() {
		return nil // no worktree to deliver
	}
	if !wt.Valid() {
		return fmt.Errorf("mgit-guest: incomplete worktree mount descriptor: %+v", wt)
	}
	// Create the identical-path mount point (the worktree's absolute host
	// path) before mounting onto it.
	if err := os.MkdirAll(wt.Path, 0o755); err != nil { //nolint:gosec // guest mount point, not host-trusted
		return fmt.Errorf("mgit-guest: create worktree mount point %q: %w", wt.Path, err)
	}
	if err := unix.Mount(wt.Source, wt.Path, wt.FSType, 0, ""); err != nil && err != unix.EBUSY {
		return fmt.Errorf("mgit-guest: mount worktree %s (%s) at %q: %w", wt.Source, wt.FSType, wt.Path, err)
	}
	return nil
}
