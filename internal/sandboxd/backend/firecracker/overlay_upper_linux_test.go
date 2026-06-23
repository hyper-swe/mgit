//go:build linux

// Proves the guest's writable-root overlay UPPER is backed by the per-VM
// disk COW drive (vdb), not guest RAM: a multi-MB write OUTSIDE the
// worktree lands on the host overlay backing file (which grows by the
// written size) and the guest's overlay scratch is a block device, not a
// tmpfs. Gated, like the other guest e2e, on a prebuilt mgit-guest rootfs
// (MGIT_E2E_GUEST_ROOTFS) + /dev/kvm. Refs: FR-17.17, NFR-17.7, MGIT-11.6.7
package firecracker

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/backend/microvm"
	"github.com/hyper-swe/mgit/internal/sandboxd/images"
)

// allocatedBytes returns the on-disk allocated size of a (possibly sparse)
// file — st_blocks * 512 — so growth reflects real disk consumption, not
// the apparent (sparse) length.
func allocatedBytes(t *testing.T, path string) int64 {
	t.Helper()
	var st syscall.Stat_t
	require.NoError(t, syscall.Stat(path, &st))
	return st.Blocks * 512
}

// TestE2E_GuestRoot_UpperOnOverlayDrive boots a real mgit-guest and writes
// a multi-MB file to a path OUTSIDE the worktree (on the writable overlay
// root). Because the overlay UPPER is backed by the disk COW drive (vdb),
// the write must consume DISK: the host overlay backing file grows by ~the
// written size, and the guest reports its overlay scratch on a block device
// rather than tmpfs. A tmpfs (RAM) upper would leave the host file's
// allocation flat. Refs: FR-17.17, NFR-17.7, MGIT-11.6.7
func TestE2E_GuestRoot_UpperOnOverlayDrive(t *testing.T) {
	kernel, _ := requireKVM(t)
	rootfs := os.Getenv("MGIT_E2E_GUEST_ROOTFS")
	if rootfs == "" {
		t.Skip("set MGIT_E2E_GUEST_ROOTFS to a guest image built with scripts/build-guest-image.sh to run the disk-backed-upper proof")
	}
	if !fileExists(rootfs) {
		t.Skipf("guest rootfs %s absent", rootfs)
	}

	wtPath := filepath.Join(t.TempDir(), "repo", "worktrees", "task-a")
	require.NoError(t, os.MkdirAll(wtPath, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(wtPath, "marker.txt"), []byte("present"), 0o600))

	clock := func() time.Time { return time.Now().UTC() }
	hostRoot := t.TempDir()
	_, err := images.GenerateTrustRoot(context.Background(), hostRoot, noopAudit{})
	require.NoError(t, err)
	priv, err := images.LoadSigningKey(hostRoot)
	require.NoError(t, err)
	entry, err := images.BuildEntry(kernel, rootfs, e2eGuestCmdline)
	require.NoError(t, err)
	ref, err := images.Register(hostRoot, "mgit-guest", entry, priv)
	require.NoError(t, err)
	store, err := images.NewStore(hostRoot, clock)
	require.NoError(t, err)

	workDir, err := os.MkdirTemp("", "mge2e") // short path for the vsock unix socket
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(workDir) })

	mgr, err := NewManager(Config{
		WorkDir: workDir,
		Resolve: func(r string) (ImagePaths, error) {
			ri, rerr := store.Resolve(r)
			return ImagePaths{KernelPath: ri.KernelPath, RootfsPath: ri.RootfsPath, Cmdline: ri.Cmdline}, rerr
		},
		Logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		Clock:  clock,
	})
	require.NoError(t, err)

	info, err := mgr.Launch(context.Background(), model.SandboxLaunchOptions{
		TaskID: "MGIT-11.6.7", WorktreePath: wtPath, ImageRef: ref,
		Network: model.NetworkPolicy{Mode: model.NetworkModeNone}, CPUs: 1, MemoryMB: 256,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Remove(context.Background(), info.ID, true) })

	// The host overlay COW backing file the guest's upper is mounted from.
	overlayImg := filepath.Join(microvm.SandboxStateDir(workDir, info.ID), "overlay.img")

	// Write 64 MiB to a path OUTSIDE the worktree (on the overlay root). The
	// MemoryMB above (256) is far above this, so a RAM/tmpfs upper would also
	// succeed — the disk proof is the host backing file's allocation growth,
	// not merely that the write succeeded.
	const writeMiB = 64
	// Write outside the worktree, then flush to the block device with an
	// explicit sync (busybox dd's conv=fsync is not portable across builds).
	// stderr is NOT suppressed so a real failure surfaces in the assertion.
	cmd := fmt.Sprintf(
		"dd if=/dev/zero of=/big.bin bs=1M count=%d && sync && "+
			"grep -E ' /mnt ' /proc/mounts", writeMiB)

	var res *model.ExecResult
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		res, err = mgr.Exec(context.Background(), info.ID, model.ExecRequest{
			Command: []string{"/bin/sh", "-c", cmd},
		})
		if err == nil {
			break
		}
		time.Sleep(400 * time.Millisecond)
	}
	require.NoError(t, err, "exec must reach the guest once it is serving vsock")
	require.Equal(t, 0, res.ExitCode, "out-of-worktree write must succeed; stderr=%q", string(res.Stderr))

	// The overlay scratch (/mnt) must be a real filesystem on a block device,
	// not tmpfs — i.e. the upper is disk-backed (vdb), not RAM.
	assert.NotContains(t, string(res.Stdout), "tmpfs",
		"/mnt (overlay upper) must NOT be tmpfs when the disk COW drive is attached")
	assert.Contains(t, string(res.Stdout), "/dev/vd",
		"/mnt (overlay upper) must be mounted from the COW block device")

	// The host overlay backing file must have grown by ~the written size:
	// the bytes the guest wrote outside the worktree landed on DISK (vdb),
	// quota-bounded, not in guest RAM. Allow filesystem overhead slack.
	grown := allocatedBytes(t, overlayImg)
	assert.GreaterOrEqual(t, grown, int64(writeMiB-8)<<20,
		"host overlay COW file must grow by ~%d MiB (disk-backed upper), got %d bytes allocated", writeMiB, grown)
}
