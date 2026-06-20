//go:build linux

// Full-stack exec round-trip against a real mgit-guest image: register a
// signed guest image, boot it under the firecracker backend, and exec a
// command over the host GuestDialer that reads the mounted worktree —
// exercising signer -> Resolve -> backend boot -> mgit-guest PID 1 ->
// worktree mount -> vsock -> guestexec, end to end. Gated on a prebuilt
// mgit-guest rootfs (scripts/build-guest-image.sh) since it is a SOUP
// artifact, like the cached CI kernel/rootfs. Refs: FR-17.11, MGIT-11.13.4
package firecracker

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/images"
)

// e2eGuestCmdline boots the mgit-guest ext4 rootfs read-only with
// mgit-guest as PID 1 (the worktree-mount descriptor is appended by the
// backend). Matches scripts/build-guest-image.sh.
const e2eGuestCmdline = "console=ttyS0 reboot=k panic=1 pci=off random.trust_cpu=on root=/dev/vda ro rootfstype=ext4 init=/sbin/mgit-guest"

// noopAudit satisfies images.TrustRootAuditor for the test trust root.
type noopAudit struct{}

func (noopAudit) RecordTrustRootChange(context.Context, string) error { return nil }

// TestE2E_Exec_RealGuest_RoundTrip proves the whole sandbox stack on real
// KVM: a signed mgit-guest image boots under the backend and an exec over
// the GuestDialer (1) reads a worktree mounted at an ARBITRARY runtime
// path the guest created on its writable overlay root (MGIT-11.6.6, not a
// path baked into the read-only image) and (2) writes to a normally
// read-only root path, proving the overlay root is writable.
func TestE2E_Exec_RealGuest_RoundTrip(t *testing.T) {
	kernel, _ := requireKVM(t) // KVM + firecracker + mke2fs + kernel present
	rootfs := os.Getenv("MGIT_E2E_GUEST_ROOTFS")
	if rootfs == "" {
		t.Skip("set MGIT_E2E_GUEST_ROOTFS to a guest image built with scripts/build-guest-image.sh to run the real round-trip")
	}
	if !fileExists(rootfs) {
		t.Skipf("guest rootfs %s absent", rootfs)
	}

	// An ARBITRARY runtime worktree path (not baked into the image): the
	// guest must create this mount point on its writable overlay root.
	wtPath := filepath.Join(t.TempDir(), "repo", "worktrees", "task-a")
	const marker = "e2e-roundtrip-marker-content"
	require.NoError(t, os.MkdirAll(wtPath, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(wtPath, "marker.txt"), []byte(marker), 0o600))

	// Register the mgit-guest image in a fresh host trust root.
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
		TaskID: "MGIT-11.13.4", WorktreePath: wtPath, ImageRef: ref,
		Network: model.NetworkPolicy{Mode: model.NetworkModeNone}, CPUs: 1, MemoryMB: 256,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Remove(context.Background(), info.ID, true) })

	// The guest boots + starts serving vsock asynchronously; retry exec
	// until the guest is listening. The command writes to a normally
	// read-only root path (proves the writable overlay root) and reads the
	// worktree mounted at its arbitrary runtime path.
	const cmd = "touch /overlay-writable-proof && cat " // + worktree marker path
	var res *model.ExecResult
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		res, err = mgr.Exec(context.Background(), info.ID, model.ExecRequest{
			Command: []string{"/bin/sh", "-c", cmd + filepath.Join(wtPath, "marker.txt")},
		})
		if err == nil {
			break
		}
		time.Sleep(400 * time.Millisecond)
	}
	require.NoError(t, err, "exec must reach the guest once it is serving vsock")
	assert.Equal(t, 0, res.ExitCode,
		"writable overlay root + worktree read both succeed (exit 0); stderr=%q", string(res.Stderr))
	assert.Contains(t, string(res.Stdout), marker,
		"the guest exec read the worktree mounted at its arbitrary runtime path")
}
