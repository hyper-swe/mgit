// Package firecracker tests verify the Linux KVM backend. The shared
// lifecycle is tested in the microvm package; this file covers the thin
// firecracker seam that builds everywhere — platform-agnostic wiring and
// CGO-free core. The real Firecracker boot path is exercised in the
// linux-only, KVM-gated tests. Refs: FR-17.15, FR-17.16
package firecracker

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/backend/microvm"
	"github.com/hyper-swe/mgit/internal/testutil"
)

// fakeVM / fakeHypervisor implement the microvm seam so the wiring can
// be exercised without a real VMM.
type fakeVM struct{}

func (fakeVM) Start(context.Context) error      { return nil }
func (fakeVM) Stop(context.Context, bool) error { return nil }

type fakeHypervisor struct{ created int }

func (h *fakeHypervisor) CreateVM(microvm.VMConfig) (microvm.VM, error) {
	h.created++
	return fakeVM{}, nil
}

func testImages(t *testing.T) ImagePaths {
	t.Helper()
	dir := t.TempDir()
	return ImagePaths{
		KernelPath: dir + "/vmlinux",
		RootfsPath: dir + "/rootfs.img",
		Cmdline:    "console=ttyS0 reboot=k panic=1 root=/dev/vda ro",
	}
}

// TestKVM_NewManager_WiresKVMBackend verifies NewManager builds a
// working manager that reports the kvm backend and uses the injected
// hypervisor. Refs: FR-17.15
func TestKVM_NewManager_WiresKVMBackend(t *testing.T) {
	hv := &fakeHypervisor{}
	mgr, err := NewManager(Config{
		WorkDir:    t.TempDir(),
		Resolve:    func(string) (ImagePaths, error) { return testImages(t), nil },
		Hypervisor: hv,
		Logger:     slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		Clock:      func() time.Time { return time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC) },
	})
	require.NoError(t, err)

	info, err := mgr.Launch(context.Background(), model.SandboxLaunchOptions{
		TaskID:       "MGIT-4.2",
		WorktreePath: "/work/MGIT-4.2",
		ImageRef:     "go-node@sha256:" + strings.Repeat("a", 64),
		Network:      model.NetworkPolicy{Mode: model.NetworkModeNone},
		CPUs:         2, MemoryMB: 1024,
	})
	require.NoError(t, err)
	assert.Equal(t, model.BackendKVM, info.Backend)
	assert.Equal(t, 1, hv.created)
}

// TestKVM_NewManager_MissingHypervisorFailsClosed verifies that with no
// injected hypervisor and an unresolvable VMM binary, NewManager fails
// closed rather than returning a manager with no isolation. Refs: SEC-04
func TestKVM_NewManager_MissingHypervisorFailsClosed(t *testing.T) {
	_, err := NewManager(Config{
		WorkDir:        t.TempDir(),
		Resolve:        func(string) (ImagePaths, error) { return testImages(t), nil },
		FirecrackerBin: "mgit-no-such-firecracker-binary-xyz",
		Logger:         slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		Clock:          func() time.Time { return time.Now().UTC() },
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrSandboxBackendUnavailable)
}

// TestKVM_CoreRemainsCGOFree verifies ADR-005 CGO containment: core
// mgit builds with CGO disabled even with the Firecracker-backed package
// in the module (the SDK is pure Go, so this must hold trivially).
// Refs: FR-17.16
func TestKVM_CoreRemainsCGOFree(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping core build in -short mode")
	}
	root := testutil.ProjectRoot(t)
	cmd := exec.CommandContext(context.Background(), "go", "build", "-o", os.DevNull, "./cmd/mgit/") //nolint:gosec // fixed argv
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "core mgit must build CGO-free: %s", out)
}
