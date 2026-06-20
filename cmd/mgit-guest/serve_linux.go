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
	// itself. /proc must come first (the worktree descriptor is read from
	// /proc/cmdline); /dev (devtmpfs) must precede the worktree mount so a
	// block-device worktree (firecracker /dev/vdc) exists to mount. The
	// mount points are created defensively for minimal rootfs images.
	if err := mountPseudoFS("proc", "/proc", "proc"); err != nil {
		return err
	}
	if err := mountPseudoFS("devtmpfs", "/dev", "devtmpfs"); err != nil {
		return err
	}
	if err := mountWorktree(); err != nil {
		return err
	}
	if err := mountPseudoFS("tmpfs", "/tmp", "tmpfs"); err != nil {
		return err
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
