// Package vzf tests verify the macOS backend wiring. The shared
// lifecycle is tested in the microvm package; here we cover the thin
// vzf seam: CGO-free core, platform selection, and (darwin) the real
// vz construction path. Refs: FR-17.15, FR-17.16
package vzf

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

// fakeVM / fakeHypervisor implement the microvm seam so the wiring can
// be exercised without the real framework.
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
		Cmdline:    "console=hvc0 root=/dev/vda ro",
	}
}

// TestVZF_NewManager_WiresVZFBackend verifies NewManager builds a
// working manager that reports the vzf backend and uses the injected
// hypervisor. Refs: FR-17.15
func TestVZF_NewManager_WiresVZFBackend(t *testing.T) {
	hv := &fakeHypervisor{}
	mgr, err := NewManager(Config{
		WorkDir:    t.TempDir(),
		Resolve:    func(string) (ImagePaths, error) { return testImages(t), nil },
		Hypervisor: hv,
		Logger:     slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		Clock:      func() time.Time { return time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC) },
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
	assert.Equal(t, model.BackendVZF, info.Backend)
	assert.Equal(t, 1, hv.created)
}

// vzfManager builds a vzf-wired manager over a fake hypervisor and
// returns it with its work dir, so lifecycle tests can inspect on-disk
// residue.
func vzfManager(t *testing.T) (mgr interface {
	Launch(context.Context, model.SandboxLaunchOptions) (*model.SandboxInfo, error)
	Remove(context.Context, string, bool) error
	Resolve(context.Context, string) (*model.SandboxInfo, error)
}, workDir string,
) {
	t.Helper()
	workDir = t.TempDir()
	m, err := NewManager(Config{
		WorkDir:    workDir,
		Resolve:    func(string) (ImagePaths, error) { return testImages(t), nil },
		Hypervisor: &fakeHypervisor{},
		Logger:     slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		Clock:      func() time.Time { return time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC) },
	})
	require.NoError(t, err)
	return m, workDir
}

func vzfLaunchOpts() model.SandboxLaunchOptions {
	return model.SandboxLaunchOptions{
		TaskID:       "MGIT-4.2",
		WorktreePath: "/work/MGIT-4.2",
		ImageRef:     "go-node@sha256:" + strings.Repeat("a", 64),
		Network:      model.NetworkPolicy{Mode: model.NetworkModeNone},
		CPUs:         2, MemoryMB: 1024,
	}
}

// TestVZF_Launch_BootsGuest verifies a launch through the vzf backend
// boots a guest (running state) and registers it. The real vz boot is
// the e2e suite's job (MGIT-11.13); here the lifecycle runs over the
// fake hypervisor. Refs: FR-17.15
func TestVZF_Launch_BootsGuest(t *testing.T) {
	mgr, workDir := vzfManager(t)

	info, err := mgr.Launch(context.Background(), vzfLaunchOpts())
	require.NoError(t, err)
	assert.Equal(t, model.BackendVZF, info.Backend)
	assert.Equal(t, model.StateRunning, info.State)
	assert.DirExists(t, filepath.Join(workDir, info.ID), "sandbox state dir exists while running")

	got, err := mgr.Resolve(context.Background(), info.ID)
	require.NoError(t, err)
	assert.Equal(t, info.ID, got.ID)
}

// TestVZF_Teardown_NoResidue verifies removing a vzf sandbox stops the
// guest and deletes every host artifact, leaving the sandbox unknown.
// Refs: FR-17.19
func TestVZF_Teardown_NoResidue(t *testing.T) {
	mgr, workDir := vzfManager(t)

	info, err := mgr.Launch(context.Background(), vzfLaunchOpts())
	require.NoError(t, err)
	dir := filepath.Join(workDir, info.ID)
	require.DirExists(t, dir)

	require.NoError(t, mgr.Remove(context.Background(), info.ID, true))
	assert.NoDirExists(t, dir, "teardown must remove every host artifact (no residue)")
	_, err = mgr.Resolve(context.Background(), info.ID)
	assert.ErrorIs(t, err, model.ErrSandboxNotFound, "removed sandbox is no longer known")
}

// TestVZF_CoreRemainsCGOFree verifies ADR-005 CGO containment: core
// mgit builds with CGO disabled even with the vz-backed package in the
// module. Refs: FR-17.16
func TestVZF_CoreRemainsCGOFree(t *testing.T) {
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
