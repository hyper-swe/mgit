// Package microvm tests verify the shared microVM lifecycle with a
// faked hypervisor (portable; runs on any platform). Real boots are
// the platform backends' integration suites. Refs: FR-17.1, FR-17.3
package microvm

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// fakeVM records lifecycle calls.
type fakeVM struct {
	mu                  sync.Mutex
	started, stopped    bool
	forced              bool
	failStart, failStop bool
}

func (v *fakeVM) Start(_ context.Context) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.failStart {
		return assert.AnError
	}
	v.started = true
	return nil
}

func (v *fakeVM) Stop(_ context.Context, force bool) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.failStop && !force {
		return assert.AnError
	}
	v.stopped, v.forced = true, force
	return nil
}

// fakeHypervisor captures the VM configs the manager builds.
type fakeHypervisor struct {
	mu           sync.Mutex
	configs      []VMConfig
	vms          []*fakeVM
	failVM       bool
	failStartVMs bool
}

func (h *fakeHypervisor) CreateVM(cfg VMConfig) (VM, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.failVM {
		return nil, assert.AnError
	}
	vm := &fakeVM{failStart: h.failStartVMs}
	h.configs = append(h.configs, cfg)
	h.vms = append(h.vms, vm)
	return vm, nil
}

func testImages(t *testing.T) ImagePaths {
	t.Helper()
	dir := t.TempDir()
	kernel := filepath.Join(dir, "vmlinux")
	rootfs := filepath.Join(dir, "rootfs.img")
	require.NoError(t, os.WriteFile(kernel, []byte("kernel"), 0o600))
	require.NoError(t, os.WriteFile(rootfs, []byte("rootfs"), 0o600))
	return ImagePaths{KernelPath: kernel, RootfsPath: rootfs, Cmdline: "console=hvc0 root=/dev/vda ro"}
}

func testManager(t *testing.T, hv Hypervisor) (*Manager, string) {
	t.Helper()
	images := testImages(t)
	workDir := t.TempDir()
	mgr, err := NewManager(Config{
		Backend: model.BackendKVM,
		WorkDir: workDir,
		Resolve: func(ref string) (ImagePaths, error) {
			if strings.Contains(ref, "unresolvable") {
				return ImagePaths{}, assert.AnError
			}
			return images, nil
		},
		Hypervisor: hv,
		Logger:     slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		Clock:      func() time.Time { return time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC) },
	})
	require.NoError(t, err)
	return mgr, workDir
}

func launchOpts(task, mode string) model.SandboxLaunchOptions {
	return model.SandboxLaunchOptions{
		TaskID:       task,
		WorktreePath: "/work/repos/mgit/worktrees/" + task,
		ImageRef:     "go-node@sha256:" + strings.Repeat("a", 64),
		Network:      model.NetworkPolicy{Mode: mode},
		CPUs:         2,
		MemoryMB:     1024,
		TTL:          time.Hour,
	}
}

// TestManager_Launch_IsolationContract verifies the VM config carries
// the FR-17 contract and the sandbox is registered before return.
func TestManager_Launch_IsolationContract(t *testing.T) {
	hv := &fakeHypervisor{}
	mgr, workDir := testManager(t, hv)
	ctx := context.Background()

	info, err := mgr.Launch(ctx, launchOpts("MGIT-4.2", model.NetworkModeNone))
	require.NoError(t, err)

	require.Len(t, hv.vms, 1)
	assert.True(t, hv.vms[0].started)
	assert.Equal(t, model.BackendKVM, info.Backend)
	assert.Equal(t, model.StateRunning, info.State)
	assert.Equal(t, 1024, info.MemoryMB)
	assert.Equal(t, info.CreatedAt.Add(time.Hour), info.ExpiresAt)

	cfg := hv.configs[0]
	assert.True(t, cfg.RootfsReadOnly, "rootfs immutable (FR-17.17)")
	assert.True(t, strings.HasPrefix(cfg.OverlayPath, workDir), "overlay never in the worktree")
	assert.FileExists(t, cfg.OverlayPath)
	assert.Equal(t, "/work/repos/mgit/worktrees/MGIT-4.2", cfg.WorktreePath, "identical-path share (FR-17.3)")
	assert.True(t, cfg.VsockEnabled)
	assert.True(t, cfg.BalloonEnabled)
	assert.False(t, cfg.AttachNIC, "none mode: no NIC (FR-17.7)")

	listed, err := mgr.List(ctx)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	assert.Equal(t, info.ID, listed[0].ID)

	t.Run("allowlist_attaches_nic", func(t *testing.T) {
		_, err := mgr.Launch(ctx, launchOpts("MGIT-4.3", model.NetworkModeAllowlist))
		require.NoError(t, err)
		assert.True(t, hv.configs[1].AttachNIC, "allowlist gets a NIC (host proxy enforces, FR-17.8)")
	})
}

// TestManager_Teardown_NoResidue verifies FR-17.19.
func TestManager_Teardown_NoResidue(t *testing.T) {
	hv := &fakeHypervisor{}
	mgr, workDir := testManager(t, hv)
	ctx := context.Background()

	worktree := t.TempDir()
	marker := filepath.Join(worktree, "landed.txt")
	require.NoError(t, os.WriteFile(marker, []byte("committed"), 0o600))
	opts := launchOpts("MGIT-4.2", model.NetworkModeNone)
	opts.WorktreePath = worktree

	info, err := mgr.Launch(ctx, opts)
	require.NoError(t, err)
	require.NoError(t, mgr.Remove(ctx, info.ID, true))

	assert.True(t, hv.vms[0].stopped)
	entries, err := os.ReadDir(workDir)
	require.NoError(t, err)
	assert.Empty(t, entries, "no sandbox-local residue (FR-17.19)")
	assert.FileExists(t, marker, "worktree untouched")

	listed, err := mgr.List(ctx)
	require.NoError(t, err)
	assert.Empty(t, listed)
	_, err = mgr.Resolve(ctx, info.ID)
	assert.ErrorIs(t, err, model.ErrSandboxNotFound)
}

// TestManager_ErrorPaths covers validation, resolver/hypervisor/start
// failures with cleanup, exec-before-transport, and lifecycle errors.
func TestManager_ErrorPaths(t *testing.T) {
	ctx := context.Background()

	t.Run("invalid_options", func(t *testing.T) {
		mgr, _ := testManager(t, &fakeHypervisor{})
		opts := launchOpts("MGIT-4.2", model.NetworkModeNone)
		opts.ImageRef = "unpinned:latest"
		_, err := mgr.Launch(ctx, opts)
		assert.Error(t, err)
	})

	t.Run("resolver_failure", func(t *testing.T) {
		mgr, _ := testManager(t, &fakeHypervisor{})
		opts := launchOpts("MGIT-4.2", model.NetworkModeNone)
		opts.ImageRef = "unresolvable@sha256:" + strings.Repeat("b", 64)
		_, err := mgr.Launch(ctx, opts)
		assert.Error(t, err)
	})

	t.Run("hypervisor_failure_cleans_overlay", func(t *testing.T) {
		mgr, workDir := testManager(t, &fakeHypervisor{failVM: true})
		_, err := mgr.Launch(ctx, launchOpts("MGIT-4.2", model.NetworkModeNone))
		require.Error(t, err)
		entries, _ := os.ReadDir(workDir)
		assert.Empty(t, entries)
	})

	t.Run("start_failure_cleans_up", func(t *testing.T) {
		mgr, workDir := testManager(t, &fakeHypervisor{failStartVMs: true})
		_, err := mgr.Launch(ctx, launchOpts("MGIT-4.2", model.NetworkModeNone))
		require.Error(t, err)
		entries, _ := os.ReadDir(workDir)
		assert.Empty(t, entries)
		listed, _ := mgr.List(ctx)
		assert.Empty(t, listed, "a sandbox that never booted is not registered")
	})

	t.Run("exec_requires_transport", func(t *testing.T) {
		mgr, _ := testManager(t, &fakeHypervisor{})
		info, err := mgr.Launch(ctx, launchOpts("MGIT-4.2", model.NetworkModeNone))
		require.NoError(t, err)
		_, err = mgr.Exec(ctx, info.ID, model.ExecRequest{Command: []string{"true"}})
		assert.Error(t, err)
		assert.NotErrorIs(t, err, model.ErrSandboxNotFound)
	})

	t.Run("unknown_ids", func(t *testing.T) {
		mgr, _ := testManager(t, &fakeHypervisor{})
		_, err := mgr.Resolve(ctx, "01JXNOPE")
		assert.ErrorIs(t, err, model.ErrSandboxNotFound)
		assert.ErrorIs(t, mgr.Stop(ctx, "01JXNOPE", false), model.ErrSandboxNotFound)
		assert.ErrorIs(t, mgr.Remove(ctx, "01JXNOPE", false), model.ErrSandboxNotFound)
		_, err = mgr.Exec(ctx, "01JXNOPE", model.ExecRequest{Command: []string{"true"}})
		assert.ErrorIs(t, err, model.ErrSandboxNotFound)
	})

	t.Run("stop_to_suspended", func(t *testing.T) {
		mgr, _ := testManager(t, &fakeHypervisor{})
		info, err := mgr.Launch(ctx, launchOpts("MGIT-4.2", model.NetworkModeNone))
		require.NoError(t, err)
		require.NoError(t, mgr.Stop(ctx, info.ID, false))
		resolved, err := mgr.Resolve(ctx, info.ID)
		require.NoError(t, err)
		assert.Equal(t, model.StateSuspended, resolved.State)
	})

	t.Run("graceful_stop_failure_then_force", func(t *testing.T) {
		hv := &fakeHypervisor{}
		mgr, _ := testManager(t, hv)
		info, err := mgr.Launch(ctx, launchOpts("MGIT-4.2", model.NetworkModeNone))
		require.NoError(t, err)
		hv.vms[0].failStop = true
		assert.Error(t, mgr.Stop(ctx, info.ID, false))
		assert.Error(t, mgr.Remove(ctx, info.ID, false))
		assert.NoError(t, mgr.Remove(ctx, info.ID, true))
	})
}

// TestManager_Guards covers constructor validation.
func TestManager_Guards(t *testing.T) {
	base := Config{
		Backend: model.BackendKVM, WorkDir: t.TempDir(),
		Resolve:    func(string) (ImagePaths, error) { return testImages(t), nil },
		Hypervisor: &fakeHypervisor{},
		Logger:     slog.New(slog.NewTextHandler(os.Stderr, nil)),
		Clock:      time.Now,
	}
	_, err := NewManager(base)
	require.NoError(t, err)

	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{name: "empty_backend", mutate: func(c *Config) { c.Backend = "" }},
		{name: "empty_workdir", mutate: func(c *Config) { c.WorkDir = "" }},
		{name: "nil_resolver", mutate: func(c *Config) { c.Resolve = nil }},
		{name: "nil_hypervisor", mutate: func(c *Config) { c.Hypervisor = nil }},
		{name: "nil_logger", mutate: func(c *Config) { c.Logger = nil }},
		{name: "nil_clock", mutate: func(c *Config) { c.Clock = nil }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := base
			tt.mutate(&cfg)
			_, err := NewManager(cfg)
			assert.Error(t, err)
		})
	}

	t.Run("unwritable_workdir", func(t *testing.T) {
		cfg := base
		blocker := filepath.Join(t.TempDir(), "blocker")
		require.NoError(t, os.WriteFile(blocker, nil, 0o600))
		cfg.WorkDir = filepath.Join(blocker, "nested")
		_, err := NewManager(cfg)
		assert.Error(t, err)
	})

	t.Run("image_digest_extraction", func(t *testing.T) {
		assert.Equal(t, "sha256:abc", imageDigest("img@sha256:abc"))
		assert.Empty(t, imageDigest("no-digest"))
	})

	t.Run("overlay_creation_failure", func(t *testing.T) {
		blocked := filepath.Join(t.TempDir(), "f")
		require.NoError(t, os.WriteFile(blocked, nil, 0o600))
		_, err := createOverlay(filepath.Join(blocked, "x"), 0)
		assert.Error(t, err)
	})
}
