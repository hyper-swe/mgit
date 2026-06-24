//go:build linux

package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"time"

	"github.com/mdlayher/vsock"
	"golang.org/x/sys/unix"

	"github.com/hyper-swe/mgit/internal/guest"
	"github.com/hyper-swe/mgit/internal/guestboot"
)

// procCmdline is the kernel command line the host appends the worktree
// descriptor to; a var so tests can supply a fixture path.
var procCmdline = "/proc/cmdline"

// serveGuest performs PID-1 duties — mount the worktree at its identical
// host path and a tmpfs /tmp — then accepts connections on two vsock ports:
// exec requests (one connection = one exec, FR-17.11) and land pulls (the
// host dials, the guest streams the task branch's object pool, SEC-01). Both
// listeners run until the context is canceled; the first that fails returns.
func serveGuest(ctx context.Context, supervisor *guest.Supervisor, execPort, landPort, notifyPort uint32, logger *slog.Logger) error {
	if err := mountGuestFilesystems(); err != nil {
		return err
	}
	worktreePath := worktreeMountPath()
	logger.Info("mgit-guest land-ready notify", "event", "notify_config", "target", describeNotify(notifyPort))

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Start one AF_VSOCK->TCP-loopback bridge per published guest port so the
	// host publisher's host->guest vsock connect reaches the guest's own dev
	// server, completing one-way port publishing (SEC-09). The ports come from
	// the kernel cmdline (guestboot); the bridges die with ctx (VM teardown),
	// so there is no goroutine leak or host residue. Refs: SEC-09, FR-17.8
	go servePublishBridges(ctx, publishPorts(), realVsockListen, realLoopbackDial, logger)

	errs := make(chan error, 2)
	go func() {
		errs <- serveVsock(ctx, execPort, logger, func(c net.Conn) {
			serveExecConn(ctx, supervisor, c, logger)
			// After the agent finishes a command, signal the host it may land
			// (auto-land trigger). Best-effort + idempotent: a host pull with no
			// new commits is a no-op, so emitting after every exec is safe and
			// gives "land as soon as done" latency. Refs: MGIT-11.10.11
			emitLandReady(notifyPort, logger)
		})
	}()
	go func() {
		errs <- serveVsock(ctx, landPort, logger, func(c net.Conn) { serveLandConn(worktreePath, c, logger) })
	}()
	// Return on first listener exit; canceling stops the other.
	err := <-errs
	cancel()
	<-errs
	return err
}

// serveVsock accepts connections on a vsock port and dispatches each to
// handle in its own goroutine until ctx is canceled.
func serveVsock(ctx context.Context, port uint32, logger *slog.Logger, handle func(net.Conn)) error {
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
				return fmt.Errorf("mgit-guest: accept :%d: %w", port, err)
			}
		}
		go handle(conn)
	}
}

// serveExecConn serves one exec connection and closes it.
func serveExecConn(ctx context.Context, supervisor *guest.Supervisor, conn net.Conn, logger *slog.Logger) {
	defer func() { _ = conn.Close() }()
	if err := supervisor.Serve(ctx, conn); err != nil {
		logger.Error("mgit-guest exec failed", "event", "exec_error", "error", err.Error())
	}
}

// serveLandConn streams the worktree's HEAD object pool to the host and
// closes the connection. With no worktree there is nothing to land.
func serveLandConn(worktreePath string, conn net.Conn, logger *slog.Logger) {
	defer func() { _ = conn.Close() }()
	if worktreePath == "" {
		return
	}
	if err := guest.ServeLandHead(worktreePath, conn); err != nil {
		logger.Error("mgit-guest land failed", "event", "land_error", "error", err.Error())
	}
}

// worktreeMountPath re-reads the kernel cmdline worktree descriptor to learn
// the worktree's absolute path (the land server's repository). Empty when no
// worktree was delivered.
func worktreeMountPath() string {
	cmdline, err := os.ReadFile(procCmdline)
	if err != nil {
		return ""
	}
	return guestboot.ParseWorktreeMount(string(cmdline)).Path
}

// publishPorts reads the kernel cmdline published-ports descriptor to learn
// which guest TCP ports to bridge over AF_VSOCK (SEC-09). Empty when no ports
// are published or the cmdline is unreadable. Refs: SEC-09, FR-17.8
func publishPorts() []int {
	cmdline, err := os.ReadFile(procCmdline)
	if err != nil {
		return nil
	}
	return guestboot.ParsePublishPorts(string(cmdline))
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

// overlayScratch is a baked empty dir the writable-root overlay's scratch
// is mounted at — the overlayfs upper + work layers and the new-root mount
// point live under it. It must exist in the read-only image. Backed either
// by the disk-backed COW overlay drive (when one is attached) or, failing
// that, by tmpfs.
const overlayScratch = "/mnt"

// scratchMounter mounts the overlay scratch at overlayScratch and reports
// whether it is disk-backed. It is a seam so the device-vs-tmpfs selection
// is unit-testable without privilege (the real mount/mkfs is e2e-gated on
// KVM). Refs: NFR-17.7, MGIT-11.6.7
type scratchMounter interface {
	// mountScratch mounts the overlay scratch over overlayScratch and
	// returns true when it is the disk overlay drive (false = tmpfs).
	mountScratch(o guestboot.OverlayUpper) (diskBacked bool, err error)
}

// makeRootWritable gives the guest a writable root over the read-only
// pinned image (FR-17.17): it overlays a writable upper on the image root
// and switch_roots into the merged view, so mgit-guest can create the
// worktree's identical mount point — and the guest can write outside the
// worktree during a build — without mutating the immutable image. The
// image stays the overlay's read-only lower.
//
// The overlay UPPER is backed by the per-VM disk-backed COW overlay drive
// (the FR-17.17/NFR-17.7 quota-bounded COW) when the host supplies an
// overlay-device descriptor on the kernel cmdline (guestboot), so large
// writes outside the worktree (npm/pip/apt) consume DISK, not guest RAM,
// and cannot OOM the guest. The drive is a raw sparse file, so it is
// mkfs-ed on first boot if unformatted. When no overlay device is supplied
// (e.g. a virtiofs backend with no drive) it falls back to a TMPFS upper.
//
// It is intentionally NOT idempotent (unlike mountPseudoFS/mountWorktree,
// which tolerate EBUSY): switch_root must run exactly once at boot, so any
// error here is fatal rather than tolerated. Refs: FR-17.3, FR-17.17, NFR-17.7, MGIT-11.6.6, MGIT-11.6.7
func makeRootWritable() error {
	cmdline, err := os.ReadFile(procCmdline)
	if err != nil {
		return fmt.Errorf("mgit-guest: read kernel cmdline: %w", err)
	}
	return makeRootWritableWith(unixScratchMounter{}, guestboot.ParseOverlayUpper(string(cmdline)))
}

// makeRootWritableWith performs the writable-root overlay + switch_root over
// an injectable scratch mounter (the disk-vs-tmpfs upper selection). The
// scratch (upper + work + new-root) is mounted first, then the image root
// is overlaid onto the new root and switch_root runs. Refs: FR-17.17, NFR-17.7
func makeRootWritableWith(m scratchMounter, o guestboot.OverlayUpper) error {
	if _, err := m.mountScratch(o); err != nil {
		return err
	}
	upper, work, newRoot := overlayScratch+"/upper", overlayScratch+"/work", overlayScratch+"/newroot"
	for _, d := range []string{upper, work, newRoot} {
		if err := os.Mkdir(d, 0o755); err != nil { //nolint:gosec // overlay scratch, not host-trusted
			return fmt.Errorf("mgit-guest: create overlay dir %s: %w", d, err)
		}
	}
	// Merge the read-only image root (lower) with the writable upper (on the
	// disk COW drive, or tmpfs when no drive is attached).
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

// mkfsCmd formats the raw overlay COW drive on first boot. The guest image
// ships busybox's mke2fs applet (CGO-free, static), which writes an ext2
// on-disk layout the guest then mounts via the kernel's ext4 driver
// (CONFIG_EXT4_FS mounts ext2/ext3/ext4) — so no e2fsprogs is needed in the
// minimal rootfs and the upper still satisfies the overlay-fs descriptor's
// declared type. A var indirection so the device-vs-tmpfs selection is
// unit-testable without execing a formatter. An absolute path: makeRootWritable
// runs as PID 1 (no inherited PATH) and before switch_root, so the formatter is
// the read-only image's /bin/mke2fs busybox applet symlink. Refs: NFR-17.7, MGIT-11.6.7
var mkfsCmd = "/bin/mke2fs"

// unixScratchMounter is the real scratch mounter. When the host supplies a
// valid disk overlay device it backs the overlay upper on that DISK (the
// quota-bounded COW drive) — mkfs-ing the raw drive on first boot if it is
// not yet a filesystem — so out-of-worktree writes consume disk, not RAM.
// When no device is supplied it falls back to a TMPFS upper. Refs: NFR-17.7
type unixScratchMounter struct{}

// mountScratch mounts the overlay scratch over overlayScratch. With a valid
// disk overlay device it mounts that device (formatting it first if a plain
// mount fails, i.e. the raw sparse drive's first boot); otherwise it mounts
// a tmpfs. Refs: FR-17.17, NFR-17.7
func (unixScratchMounter) mountScratch(o guestboot.OverlayUpper) (bool, error) {
	if !o.Valid() {
		if err := unix.Mount("tmpfs", overlayScratch, "tmpfs", 0, ""); err != nil {
			return false, fmt.Errorf("mgit-guest: mount tmpfs overlay scratch %s: %w", overlayScratch, err)
		}
		return false, nil
	}
	// First boot: the COW drive is a raw sparse file with no filesystem, so a
	// plain mount fails; format it once, then mount. A subsequent boot (drive
	// already formatted) mounts directly.
	if err := unix.Mount(o.Device, overlayScratch, o.FSType, 0, ""); err != nil {
		if mErr := formatOverlayDrive(o.Device); mErr != nil {
			return false, mErr
		}
		if err := unix.Mount(o.Device, overlayScratch, o.FSType, 0, ""); err != nil {
			return false, fmt.Errorf("mgit-guest: mount overlay drive %s after mkfs: %w", o.Device, err)
		}
	}
	return true, nil
}

// formatMkfsTimeout bounds the one-shot first-boot mkfs of the COW drive so
// a hung formatter cannot wedge boot indefinitely.
const formatMkfsTimeout = 60 * time.Second

// formatOverlayDrive runs mkfs on the raw overlay COW drive (the busybox
// mke2fs applet in the guest image). It is invoked once, on first boot, when
// the sparse drive has no filesystem yet. Refs: NFR-17.7, MGIT-11.6.7
func formatOverlayDrive(device string) error {
	ctx, cancel := context.WithTimeout(context.Background(), formatMkfsTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, mkfsCmd, "-F", device) //nolint:gosec // device path is host-supplied via the signed-image cmdline, no shell
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("mgit-guest: mkfs overlay drive %s: %w: %s", device, err, out)
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
