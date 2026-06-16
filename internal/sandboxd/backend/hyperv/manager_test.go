// Package hyperv tests verify the placeholder fails closed and the
// prerequisites are documented. No Windows backend ships in v1 (ADR-006,
// FR-17.39); the real WCOW backend is epic MGIT-12. Refs: FR-17.15, FR-17.39
package hyperv

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
	"github.com/hyper-swe/mgit/internal/sandboxd/backend/microvm"
)

// fakeVM / fakeHost implement the microvm seam so the hyperv wiring can
// be exercised without a real Hyper-V host.
type fakeVM struct{ stopped bool }

func (v *fakeVM) Start(context.Context) error { return nil }
func (v *fakeVM) Stop(context.Context, bool) error {
	v.stopped = true
	return nil
}

type fakeHost struct{ vms []*fakeVM }

func (h *fakeHost) CreateVM(microvm.VMConfig) (microvm.VM, error) {
	vm := &fakeVM{}
	h.vms = append(h.vms, vm)
	return vm, nil
}

func testManager(t *testing.T, host microvm.Hypervisor) *microvm.Manager {
	t.Helper()
	dir := t.TempDir()
	mgr, err := NewManager(Config{
		WorkDir: t.TempDir(),
		Resolve: func(string) (ImagePaths, error) {
			return ImagePaths{KernelPath: dir + "/k", RootfsPath: dir + "/r", Cmdline: "console=hvc0"}, nil
		},
		Hypervisor: host,
		Logger:     slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		Clock:      func() time.Time { return time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC) },
	})
	require.NoError(t, err)
	return mgr
}

func hyperVOpts() model.SandboxLaunchOptions {
	return model.SandboxLaunchOptions{
		TaskID:       "MGIT-4.2",
		WorktreePath: "/work/MGIT-4.2",
		ImageRef:     "go-node@sha256:" + strings.Repeat("a", 64),
		Network:      model.NetworkPolicy{Mode: model.NetworkModeNone},
		CPUs:         2, MemoryMB: 1024, TTL: time.Hour,
	}
}

// TestHyperV_NewManager_WiresBackend verifies NewManager builds a
// manager reporting the hyperv backend over the injected host (the
// wiring; a real guest boot is runner-gated). Refs: FR-17.15
func TestHyperV_NewManager_WiresBackend(t *testing.T) {
	host := &fakeHost{}
	mgr := testManager(t, host)

	info, err := mgr.Launch(context.Background(), hyperVOpts())
	require.NoError(t, err)
	assert.Equal(t, model.BackendHyperV, info.Backend)
	require.Len(t, host.vms, 1)
	assert.Equal(t, model.StateRunning, info.State)
}

// TestHyperV_Teardown_NoResidue verifies teardown stops the VM and
// deregisters via the shared lifecycle. Refs: FR-17.19
func TestHyperV_Teardown_NoResidue(t *testing.T) {
	host := &fakeHost{}
	mgr := testManager(t, host)
	ctx := context.Background()

	info, err := mgr.Launch(ctx, hyperVOpts())
	require.NoError(t, err)
	require.NoError(t, mgr.Remove(ctx, info.ID, true))

	assert.True(t, host.vms[0].stopped)
	listed, err := mgr.List(ctx)
	require.NoError(t, err)
	assert.Empty(t, listed)
}

// TestHyperV_PrereqsDocumented verifies the prerequisites are
// documented and cover the essentials. Refs: FR-17.15
func TestHyperV_PrereqsDocumented(t *testing.T) {
	for _, want := range []string{"Hyper-V", "Administrator", "virtualization", "reboot"} {
		assert.Contains(t, Prerequisites, want,
			"prerequisites must document %q", want)
	}
}

// TestHyperV_PlatformHost_FailsClosed verifies that with no host wired
// (nil Config.Hypervisor), the backend refuses rather than fabricating a
// VM — no Windows backend ships in v1 (deferred to MGIT-12). Refs: FR-17.39
func TestHyperV_PlatformHost_FailsClosed(t *testing.T) {
	_, err := NewManager(Config{
		WorkDir: t.TempDir(),
		Resolve: func(string) (ImagePaths, error) { return ImagePaths{}, nil },
		Logger:  slog.New(slog.NewTextHandler(os.Stderr, nil)),
		Clock:   time.Now,
	})
	assert.ErrorIs(t, err, model.ErrSandboxBackendUnavailable)
}
