//go:build linux

// Real Firecracker tests. The drive-contract test is pure construction
// and always runs; the boot/teardown tests need /dev/kvm, the
// firecracker binary, and a kernel+rootfs, and skip cleanly when any is
// absent (so non-KVM CI stays green). On a KVM runner they boot a real
// guest and prove the FR-17 isolation contract end to end.
// Refs: FR-17.15, FR-17.17, FR-17.19
package firecracker

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/backend/microvm"
	"github.com/hyper-swe/mgit/internal/testutil"
)

// bootCmdline boots the read-only squashfs root with the writable
// overlay on vdb via the CI image's overlay-init.
const bootCmdline = "console=ttyS0 reboot=k panic=1 pci=off random.trust_cpu=on init=/sbin/overlay-init"

// testImagePaths locates the guest kernel + rootfs, preferring the env
// overrides and falling back to the repo's .testdata/images cache.
func testImagePaths(t *testing.T) (kernel, rootfs string, ok bool) {
	t.Helper()
	kernel = os.Getenv("MGIT_TEST_KERNEL")
	rootfs = os.Getenv("MGIT_TEST_ROOTFS")
	if kernel == "" || rootfs == "" {
		base := filepath.Join(testutil.ProjectRoot(t), ".testdata", "images")
		kernel = filepath.Join(base, "vmlinux-5.10.223")
		rootfs = filepath.Join(base, "ubuntu-22.04.squashfs")
	}
	if !fileExists(kernel) || !fileExists(rootfs) {
		return "", "", false
	}
	return kernel, rootfs, true
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil } //nolint:gosec // test fixture path

// requireKVM skips unless this host can actually boot a Firecracker
// guest: /dev/kvm usable, the firecracker binary present, images cached.
func requireKVM(t *testing.T) (kernel, rootfs string) {
	t.Helper()
	if f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0); err != nil {
		t.Skipf("no usable /dev/kvm: %v", err)
	} else {
		_ = f.Close()
	}
	if _, err := newPlatformHypervisor(""); err != nil {
		t.Skipf("firecracker backend unavailable: %v", err)
	}
	kernel, rootfs, ok := testImagePaths(t)
	if !ok {
		t.Skip("guest images absent (set MGIT_TEST_KERNEL/MGIT_TEST_ROOTFS or populate .testdata/images)")
	}
	return kernel, rootfs
}

// kvmManager builds a real Firecracker-backed manager over a short-path
// work dir (unix socket paths are length-limited).
func kvmManager(t *testing.T, kernel, rootfs string) (*microvm.Manager, string) {
	t.Helper()
	workDir, err := os.MkdirTemp("", "mgfc")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(workDir) })
	mgr, err := NewManager(Config{
		WorkDir: workDir,
		Resolve: func(string) (ImagePaths, error) {
			return ImagePaths{KernelPath: kernel, RootfsPath: rootfs, Cmdline: bootCmdline}, nil
		},
		Logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		Clock:  func() time.Time { return time.Now().UTC() },
	})
	require.NoError(t, err)
	return mgr, workDir
}

func launchOpts() model.SandboxLaunchOptions {
	return model.SandboxLaunchOptions{
		TaskID:       "MGIT-11.5.1",
		WorktreePath: "/work/MGIT-11.5.1",
		ImageRef:     "fc-ci@sha256:" + strings.Repeat("a", 64),
		Network:      model.NetworkPolicy{Mode: model.NetworkModeNone},
		CPUs:         2, MemoryMB: 512,
	}
}

// TestKVM_Launch_BootsGuest boots a real guest and proves the kernel
// ran by waiting for its boot banner on the serial console, recording
// cold-boot latency against the NFR-17.4 target. Refs: FR-17.15, NFR-17.4
func TestKVM_Launch_BootsGuest(t *testing.T) {
	kernel, rootfs := requireKVM(t)
	mgr, workDir := kvmManager(t, kernel, rootfs)

	info, err := mgr.Launch(context.Background(), launchOpts())
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Remove(context.Background(), info.ID, true) })
	assert.Equal(t, model.BackendKVM, info.Backend)
	assert.Equal(t, model.StateRunning, info.State)

	console := filepath.Join(workDir, info.ID, "console.log")
	booted, elapsed := waitForConsole(t, console, "Linux version", 20*time.Second)
	require.True(t, booted, "guest kernel must boot (no 'Linux version' banner on serial console)")
	t.Logf("cold boot latency to kernel banner: %v (NFR-17.4 target <1s warm path; cold first-boot)", elapsed)
}

// TestKVM_RootfsReadOnly_OverlayCOW asserts the drive contract: the
// pinned image is the read-only root device and the per-VM overlay is a
// separate writable device (the COW layer). Refs: FR-17.17
func TestKVM_RootfsReadOnly_OverlayCOW(t *testing.T) {
	cfg := microvm.VMConfig{
		CPUs: 2, MemoryMB: 512,
		KernelPath: "/img/vmlinux", RootfsPath: "/img/rootfs.sqfs", RootfsReadOnly: true,
		Cmdline: bootCmdline, OverlayPath: "/state/sb1/overlay.img",
		VsockEnabled: true,
	}
	fcfg := buildConfig(cfg, sandboxPaths(cfg.OverlayPath))

	require.Len(t, fcfg.Drives, 2, "exactly a rootfs + overlay drive")
	root := fcfg.Drives[0]
	assert.True(t, *root.IsRootDevice, "rootfs is the root device")
	assert.True(t, *root.IsReadOnly, "pinned rootfs must be read-only (FR-17.17)")
	assert.Equal(t, cfg.RootfsPath, *root.PathOnHost)

	overlay := fcfg.Drives[1]
	assert.False(t, *overlay.IsRootDevice, "overlay is not the root device")
	assert.False(t, *overlay.IsReadOnly, "overlay is the writable COW layer")
	assert.Equal(t, cfg.OverlayPath, *overlay.PathOnHost)
	assert.NotEqual(t, *root.PathOnHost, *overlay.PathOnHost, "rootfs and overlay are distinct backing files")

	require.Len(t, fcfg.VsockDevices, 1, "vsock control plane present")
	assert.Equal(t, uint32(guestVsockCID), fcfg.VsockDevices[0].CID)
}

// TestKVM_Teardown_NoResidue boots a guest, removes it, and verifies the
// entire per-sandbox state dir (overlay, sockets, console) is gone and
// the sandbox is no longer known — the worktree is never touched.
// Refs: FR-17.19
func TestKVM_Teardown_NoResidue(t *testing.T) {
	kernel, rootfs := requireKVM(t)
	mgr, workDir := kvmManager(t, kernel, rootfs)

	info, err := mgr.Launch(context.Background(), launchOpts())
	require.NoError(t, err)
	dir := filepath.Join(workDir, info.ID)
	require.DirExists(t, dir, "sandbox state dir exists while running")

	require.NoError(t, mgr.Remove(context.Background(), info.ID, true))
	assert.NoDirExists(t, dir, "teardown must remove every host artifact (no residue)")
	_, err = mgr.Resolve(context.Background(), info.ID)
	assert.ErrorIs(t, err, model.ErrSandboxNotFound, "removed sandbox is no longer known")
}

// TestKVM_BuildConfig_VsockDisabled covers the no-vsock branch: when
// the control plane is disabled the config carries no vsock device.
func TestKVM_BuildConfig_VsockDisabled(t *testing.T) {
	cfg := microvm.VMConfig{
		CPUs: 1, MemoryMB: 256,
		KernelPath: "/img/vmlinux", RootfsPath: "/img/rootfs.sqfs", RootfsReadOnly: true,
		OverlayPath: "/state/sb1/overlay.img", VsockEnabled: false,
	}
	fcfg := buildConfig(cfg, sandboxPaths(cfg.OverlayPath))
	assert.Empty(t, fcfg.VsockDevices, "no vsock device when the control plane is disabled")
}

// TestKVM_SandboxPaths_AllUnderStateDir verifies every host artifact is
// rooted in the sandbox state dir so teardown is one RemoveAll.
// Refs: FR-17.19
func TestKVM_SandboxPaths_AllUnderStateDir(t *testing.T) {
	p := sandboxPaths("/state/sb1/overlay.img")
	for _, path := range []string{p.socket, p.vsock, p.console} {
		assert.Equal(t, "/state/sb1", filepath.Dir(path), "artifact %q must live under the state dir", path)
	}
}

// TestKVM_NewPlatformHypervisor_MissingBinary covers the fail-closed
// branch: an absent firecracker binary yields ErrSandboxBackendUnavailable
// rather than a silent downgrade. Refs: FR-17.15, SEC-04
func TestKVM_NewPlatformHypervisor_MissingBinary(t *testing.T) {
	_, err := newPlatformHypervisor("mgit-no-such-firecracker-binary-xyz")
	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrSandboxBackendUnavailable)
}

// TestKVM_NewPlatformHypervisor_NoKVMDevice covers the second fail-closed
// branch: the binary is present but /dev/kvm is absent. Refs: FR-17.15, SEC-04
func TestKVM_NewPlatformHypervisor_NoKVMDevice(t *testing.T) {
	if _, err := exec.LookPath("firecracker"); err != nil {
		t.Skip("firecracker binary absent")
	}
	orig := kvmDevice
	t.Cleanup(func() { kvmDevice = orig })
	kvmDevice = filepath.Join(t.TempDir(), "absent-kvm")

	_, err := newPlatformHypervisor("")
	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrSandboxBackendUnavailable)
}

// TestKVM_CreateVM_InvalidConfigSurfaces covers the validation branch:
// a config the VMM would reject fails at Validate, before any process is
// spawned. Constructs the hypervisor directly so it needs no /dev/kvm.
func TestKVM_CreateVM_InvalidConfigSurfaces(t *testing.T) {
	hv := &fcHypervisor{bin: "firecracker"}
	_, err := hv.CreateVM(microvm.VMConfig{
		CPUs: 1, MemoryMB: 256,
		KernelPath:  filepath.Join(t.TempDir(), "absent-kernel"),
		RootfsPath:  filepath.Join(t.TempDir(), "absent-rootfs"),
		OverlayPath: filepath.Join(t.TempDir(), "overlay.img"),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "config invalid", "missing image files must fail at validation")
}

// requireFirecracker skips unless /dev/kvm and the firecracker binary
// are usable, returning the constructed hypervisor. Unlike requireKVM it
// needs no guest images.
func requireFirecracker(t *testing.T) *fcHypervisor {
	t.Helper()
	hv, err := newPlatformHypervisor("")
	if err != nil {
		t.Skipf("firecracker backend unavailable: %v", err)
	}
	return hv.(*fcHypervisor)
}

// TestKVM_Start_BadKernelSurfaces covers the start-error path: image
// files that exist (so config validation passes) but are not a real
// kernel must surface as a start failure, not a silent success.
func TestKVM_Start_BadKernelSurfaces(t *testing.T) {
	hv := requireFirecracker(t)
	dir := t.TempDir()
	for _, n := range []string{"vmlinux", "rootfs", "overlay.img"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, n), []byte("not a real kernel image"), 0o600))
	}
	vm, err := hv.CreateVM(microvm.VMConfig{
		CPUs: 1, MemoryMB: 128,
		KernelPath: filepath.Join(dir, "vmlinux"),
		RootfsPath: filepath.Join(dir, "rootfs"), RootfsReadOnly: true,
		OverlayPath: filepath.Join(dir, "overlay.img"),
		Cmdline:     bootCmdline, VsockEnabled: true,
	})
	require.NoError(t, err, "construction succeeds; the bogus kernel is only rejected at boot")
	err = vm.Start(context.Background())
	require.Error(t, err, "a bogus kernel must surface as a start error")
	assert.Contains(t, err.Error(), "firecracker start")
}

// TestKVM_CreateVM_ConsoleOpenError covers the console-creation error
// branch: a state dir the process cannot write to fails before any VMM
// process is spawned. Skips when run as root (root ignores the mode).
func TestKVM_CreateVM_ConsoleOpenError(t *testing.T) {
	hv := requireFirecracker(t)
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory write permissions")
	}
	dir := t.TempDir()
	for _, n := range []string{"vmlinux", "rootfs", "overlay.img"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600))
	}
	require.NoError(t, os.Chmod(dir, 0o500))       //nolint:gosec // intentional: an unwritable state dir
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) //nolint:gosec // restore so t.TempDir cleanup can remove it

	_, err := hv.CreateVM(microvm.VMConfig{
		CPUs: 1, MemoryMB: 128,
		KernelPath: filepath.Join(dir, "vmlinux"),
		RootfsPath: filepath.Join(dir, "rootfs"), RootfsReadOnly: true,
		OverlayPath: filepath.Join(dir, "overlay.img"), Cmdline: bootCmdline,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "console", "an unwritable state dir fails at console creation")
}

// TestKVM_Remove_GracefulStop boots a guest and removes it without
// force, exercising the graceful-shutdown path. Refs: FR-17.19
func TestKVM_Remove_GracefulStop(t *testing.T) {
	kernel, rootfs := requireKVM(t)
	mgr, workDir := kvmManager(t, kernel, rootfs)

	info, err := mgr.Launch(context.Background(), launchOpts())
	require.NoError(t, err)
	require.NoError(t, mgr.Remove(context.Background(), info.ID, false))
	assert.NoDirExists(t, filepath.Join(workDir, info.ID), "graceful teardown also leaves no residue")
}

// waitForConsole polls a serial-console capture file for a marker.
func waitForConsole(t *testing.T, path, marker string, timeout time.Duration) (bool, time.Duration) {
	t.Helper()
	start := time.Now()
	deadline := start.Add(timeout)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil && strings.Contains(string(data), marker) { //nolint:gosec // test path
			return true, time.Since(start)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false, time.Since(start)
}
