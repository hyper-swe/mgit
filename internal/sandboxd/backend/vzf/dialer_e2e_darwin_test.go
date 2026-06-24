//go:build darwin && cgo

// Real-VM exec round-trip over the vzf GuestDialer: boot a guest under
// Virtualization.framework and exec a command that the shared manager
// routes through the dialer's VZVirtioSocketDevice.Connect. It proves the
// whole macOS transport — live VM handle -> framework vsock connect ->
// guest exec — composes, the counterpart of firecracker's
// roundtrip_linux_test.go. It is gated like that suite: it needs the
// com.apple.security.virtualization entitlement (a signed test binary) and
// a guest image that serves mgit-guest on the exec vsock port, so without
// them it skips rather than fails. Refs: FR-17.11, FR-17.16, MGIT-11.13
package vzf

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// e2eVZFCmdline is the kernel cmdline the vzf live-e2e guests boot with. Like
// firecracker's e2eGuestCmdline it MUST set init=/sbin/mgit-guest (else the
// kernel falls through to /bin/sh as PID 1 and nothing serves vsock) and
// rootfstype=ext4; console=hvc0 is the vz virtio-console (no ttyS0/pci=off —
// vz presents virtio over PCI, not firecracker's MMIO). Refs: MGIT-13.1.1
const e2eVZFCmdline = "console=hvc0 root=/dev/vda ro rootfstype=ext4 init=/sbin/mgit-guest"

// TestE2E_VZF_Exec_RealGuest_RoundTrip boots a real guest and execs over
// the dialer. Gated on MGIT_E2E_VZF_KERNEL + MGIT_E2E_GUEST_ROOTFS and the
// virtualization entitlement.
func TestE2E_VZF_Exec_RealGuest_RoundTrip(t *testing.T) {
	kernel := os.Getenv("MGIT_E2E_VZF_KERNEL")
	rootfs := os.Getenv("MGIT_E2E_GUEST_ROOTFS")
	if kernel == "" || rootfs == "" {
		t.Skip("set MGIT_E2E_VZF_KERNEL and MGIT_E2E_GUEST_ROOTFS (a guest image serving mgit-guest on the exec vsock port) to run the vzf round-trip")
	}
	for _, p := range []string{kernel, rootfs} {
		if !fileExists(p) {
			t.Skipf("guest image %s absent", p)
		}
	}

	wtPath := t.TempDir()
	mgr, err := NewManager(Config{
		WorkDir: t.TempDir(),
		Resolve: func(string) (ImagePaths, error) {
			return ImagePaths{KernelPath: kernel, RootfsPath: rootfs, Cmdline: e2eVZFCmdline}, nil
		},
		Logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		Clock:  func() time.Time { return time.Now().UTC() },
	})
	require.NoError(t, err)

	info, err := mgr.Launch(context.Background(), model.SandboxLaunchOptions{
		TaskID: "MGIT-13.1", WorktreePath: wtPath,
		ImageRef: "mgit-guest@sha256:" + strings.Repeat("a", 64),
		Network:  model.NetworkPolicy{Mode: model.NetworkModeNone}, CPUs: 1, MemoryMB: 512,
	})
	if err != nil && strings.Contains(err.Error(), "com.apple.security.virtualization") {
		t.Skipf("test binary lacks the virtualization entitlement: %v", err)
	}
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Remove(context.Background(), info.ID, true) })

	// The guest boots and starts serving vsock asynchronously; retry the
	// exec (which dials through the GuestDialer) until it is listening.
	var res *model.ExecResult
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		res, err = mgr.Exec(context.Background(), info.ID, model.ExecRequest{
			Command: []string{"/bin/sh", "-c", "echo vzf-roundtrip-ok"},
		})
		if err == nil {
			break
		}
		time.Sleep(400 * time.Millisecond)
	}
	require.NoError(t, err, "exec must reach the guest over the framework vsock once it is serving")
	assert.Equal(t, 0, res.ExitCode, "stderr=%q", string(res.Stderr))
	assert.Contains(t, string(res.Stdout), "vzf-roundtrip-ok",
		"the guest exec ran over the dialer's VZVirtioSocketDevice.Connect")
}

// fileExists reports whether an operator-supplied test image path exists.
func fileExists(p string) bool { _, err := os.Stat(p); return err == nil } //nolint:gosec // test fixture path
