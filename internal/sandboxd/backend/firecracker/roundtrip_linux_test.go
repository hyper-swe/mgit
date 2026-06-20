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
// the GuestDialer reads the worktree mounted into the guest.
func TestE2E_Exec_RealGuest_RoundTrip(t *testing.T) {
	kernel, _ := requireKVM(t) // KVM + firecracker + mke2fs + kernel present
	rootfs := os.Getenv("MGIT_E2E_GUEST_ROOTFS")
	wtPath := os.Getenv("MGIT_E2E_WORKTREE")
	if rootfs == "" || wtPath == "" {
		t.Skip("set MGIT_E2E_GUEST_ROOTFS (build with scripts/build-guest-image.sh) and " +
			"MGIT_E2E_WORKTREE (the worktree mount point baked into that image) to run the real round-trip")
	}
	if !fileExists(rootfs) {
		t.Skipf("guest rootfs %s absent", rootfs)
	}

	// The host worktree the backend packs into the guest (vdc), with a marker.
	const marker = "e2e-roundtrip-marker-content"
	require.NoError(t, os.MkdirAll(wtPath, 0o755))                                               //nolint:gosec // operator-supplied test worktree dir
	require.NoError(t, os.WriteFile(filepath.Join(wtPath, "marker.txt"), []byte(marker), 0o600)) //nolint:gosec // operator-supplied test worktree path

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
	// until the guest is listening, then assert it read the mounted worktree.
	var res *model.ExecResult
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		res, err = mgr.Exec(context.Background(), info.ID, model.ExecRequest{
			Command: []string{"/bin/sh", "-c", "cat " + filepath.Join(wtPath, "marker.txt")},
		})
		if err == nil {
			break
		}
		time.Sleep(400 * time.Millisecond)
	}
	require.NoError(t, err, "exec must reach the guest once it is serving vsock")
	assert.Equal(t, 0, res.ExitCode)
	assert.Contains(t, string(res.Stdout), marker,
		"the guest exec read the worktree mounted over the real round-trip")
}
