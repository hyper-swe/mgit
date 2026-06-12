// Package vzf tests verify the macOS Virtualization.framework backend
// per MGIT-11.5.2 acceptance criteria. The hypervisor seam is faked:
// real VM boots need a signed binary with the virtualization
// entitlement plus guest images, exercised by the e2e suite
// (MGIT-11.13.1) on a configured runner. Refs: FR-17.15, FR-17.3
package vzf

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	mu        sync.Mutex
	started   bool
	stopped   bool
	forced    bool
	failStart bool
	failStop  bool
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

// fakeHypervisor captures the VM configuration the manager builds.
type fakeHypervisor struct {
	mu           sync.Mutex
	configs      []vmConfig
	vms          []*fakeVM
	failVM       bool
	failStartVMs bool
}

func (h *fakeHypervisor) CreateVM(cfg vmConfig) (virtualMachine, error) {
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

func testImages(t *testing.T) (ImagePaths, string) {
	t.Helper()
	dir := t.TempDir()
	kernel := filepath.Join(dir, "vmlinux")
	rootfs := filepath.Join(dir, "rootfs.img")
	require.NoError(t, os.WriteFile(kernel, []byte("kernel"), 0o600))
	require.NoError(t, os.WriteFile(rootfs, []byte("rootfs"), 0o600))
	return ImagePaths{
		KernelPath: kernel,
		RootfsPath: rootfs,
		Cmdline:    "console=hvc0 root=/dev/vda ro",
	}, dir
}

func testManager(t *testing.T, hv hypervisor) (*Manager, string) {
	t.Helper()
	images, _ := testImages(t)
	workDir := t.TempDir()
	mgr, err := NewManager(Config{
		WorkDir: workDir,
		Resolve: func(imageRef string) (ImagePaths, error) {
			if strings.Contains(imageRef, "unresolvable") {
				return ImagePaths{}, assert.AnError
			}
			return images, nil
		},
		Hypervisor: hv,
		Logger:     slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		Clock:      func() time.Time { return time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC) },
	})
	require.NoError(t, err)
	return mgr, workDir
}

func vzLaunchOpts(task string) model.SandboxLaunchOptions {
	return model.SandboxLaunchOptions{
		TaskID:       task,
		WorktreePath: "/work/repos/mgit/worktrees/" + task,
		ImageRef:     "go-node@sha256:" + strings.Repeat("a", 64),
		Network:      model.NetworkPolicy{Mode: model.NetworkModeNone},
		CPUs:         2,
		MemoryMB:     1024,
		TTL:          time.Hour,
	}
}

// TestVZF_Launch_BootsGuest verifies the boot orchestration: the
// manager resolves the pinned image, builds a config carrying the
// FR-17 isolation contract (read-only rootfs, per-sandbox COW overlay,
// worktree share at the identical path, vsock, balloon), starts the
// VM, and registers it before returning. Refs: FR-17.3, FR-17.15, FR-17.17
func TestVZF_Launch_BootsGuest(t *testing.T) {
	hv := &fakeHypervisor{}
	mgr, workDir := testManager(t, hv)
	ctx := context.Background()

	info, err := mgr.Launch(ctx, vzLaunchOpts("MGIT-4.2"))
	require.NoError(t, err)

	require.Len(t, hv.vms, 1)
	assert.True(t, hv.vms[0].started, "the guest must be booted")
	assert.Equal(t, model.BackendVZF, info.Backend)
	assert.Equal(t, model.StateRunning, info.State)
	assert.Equal(t, "MGIT-4.2", info.TaskID)
	assert.Equal(t, 1024, info.MemoryMB, "effective memory recorded for the ceiling")
	assert.Equal(t, info.CreatedAt.Add(time.Hour), info.ExpiresAt, "TTL deadline recorded")

	cfg := hv.configs[0]
	assert.True(t, cfg.RootfsReadOnly, "rootfs is immutable (FR-17.17)")
	assert.True(t, strings.HasPrefix(cfg.OverlayPath, workDir),
		"COW overlay lives in the manager work dir, never the worktree")
	assert.FileExists(t, cfg.OverlayPath, "overlay backing file created")
	assert.Equal(t, "/work/repos/mgit/worktrees/MGIT-4.2", cfg.WorktreePath,
		"the worktree is shared at the identical path (FR-17.3)")
	assert.True(t, cfg.VsockEnabled, "vsock control plane always present")
	assert.True(t, cfg.BalloonEnabled, "memory balloon for NFR-17.4")
	assert.False(t, cfg.AttachNIC, "mode none attaches no NIC (FR-17.7)")
	assert.Equal(t, 2, cfg.CPUs)
	assert.Equal(t, 1024, cfg.MemoryMB)

	t.Run("registered_in_list_before_return", func(t *testing.T) {
		listed, err := mgr.List(ctx)
		require.NoError(t, err)
		require.Len(t, listed, 1)
		assert.Equal(t, info.ID, listed[0].ID)
	})

	t.Run("nic_attached_for_allowlist_mode", func(t *testing.T) {
		opts := vzLaunchOpts("MGIT-4.3")
		opts.Network = model.NetworkPolicy{Mode: model.NetworkModeAllowlist}
		_, err := mgr.Launch(ctx, opts)
		require.NoError(t, err)
		assert.True(t, hv.configs[1].AttachNIC,
			"allowlist mode gets a NIC (host proxy enforces the policy, FR-17.8)")
	})
}

// TestVZF_Teardown_NoResidue verifies FR-17.19: removing a sandbox
// stops the VM, deletes the overlay and every sandbox-local file, and
// leaves the worktree untouched.
func TestVZF_Teardown_NoResidue(t *testing.T) {
	hv := &fakeHypervisor{}
	mgr, workDir := testManager(t, hv)
	ctx := context.Background()

	worktree := t.TempDir()
	marker := filepath.Join(worktree, "landed-work.txt")
	require.NoError(t, os.WriteFile(marker, []byte("committed"), 0o600))

	opts := vzLaunchOpts("MGIT-4.2")
	opts.WorktreePath = worktree
	info, err := mgr.Launch(ctx, opts)
	require.NoError(t, err)

	require.NoError(t, mgr.Remove(ctx, info.ID, true))

	assert.True(t, hv.vms[0].stopped, "the VM is stopped at teardown")
	entries, err := os.ReadDir(workDir)
	require.NoError(t, err)
	assert.Empty(t, entries, "no sandbox-local files survive teardown (FR-17.19)")
	assert.FileExists(t, marker, "the worktree is never touched by teardown")

	listed, err := mgr.List(ctx)
	require.NoError(t, err)
	assert.Empty(t, listed, "destroyed sandboxes are deregistered")

	_, err = mgr.Resolve(ctx, info.ID)
	assert.ErrorIs(t, err, model.ErrSandboxNotFound)
}

// TestVZF_CoreRemainsCGOFree verifies the ADR-005 CGO containment:
// core mgit builds with CGO disabled even with the vz-backed package
// in the module. Refs: FR-17.16
func TestVZF_CoreRemainsCGOFree(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping core build in -short mode")
	}
	root := projectRoot(t)
	cmd := exec.CommandContext(context.Background(), "go", "build", "-o", os.DevNull, "./cmd/mgit/") //nolint:gosec // OK: fixed argv, no user input
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "core mgit must build CGO-free: %s", out)
}

// projectRoot walks up from this file to the module root.
func projectRoot(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	require.True(t, ok)
	dir := filepath.Dir(filename)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		require.NotEqual(t, dir, parent, "go.mod not found")
		dir = parent
	}
}

// TestVZF_ErrorPaths covers launch validation, resolver failures,
// hypervisor failures (with overlay cleanup), exec-before-transport,
// and lifecycle errors.
func TestVZF_ErrorPaths(t *testing.T) {
	ctx := context.Background()

	t.Run("invalid_options_rejected", func(t *testing.T) {
		mgr, _ := testManager(t, &fakeHypervisor{})
		opts := vzLaunchOpts("MGIT-4.2")
		opts.ImageRef = "unpinned:latest"
		_, err := mgr.Launch(ctx, opts)
		assert.Error(t, err)
	})

	t.Run("resolver_failure_propagates", func(t *testing.T) {
		mgr, _ := testManager(t, &fakeHypervisor{})
		opts := vzLaunchOpts("MGIT-4.2")
		opts.ImageRef = "unresolvable@sha256:" + strings.Repeat("b", 64)
		_, err := mgr.Launch(ctx, opts)
		assert.Error(t, err)
	})

	t.Run("hypervisor_failure_cleans_overlay", func(t *testing.T) {
		hv := &fakeHypervisor{failVM: true}
		mgr, workDir := testManager(t, hv)
		_, err := mgr.Launch(ctx, vzLaunchOpts("MGIT-4.2"))
		require.Error(t, err)
		entries, err := os.ReadDir(workDir)
		require.NoError(t, err)
		assert.Empty(t, entries, "a failed launch leaves no residue")
	})

	t.Run("start_failure_cleans_up", func(t *testing.T) {
		mgr, workDir := testManager(t, &fakeHypervisor{failStartVMs: true})
		_, err := mgr.Launch(ctx, vzLaunchOpts("MGIT-4.2"))
		require.Error(t, err)
		entries, err := os.ReadDir(workDir)
		require.NoError(t, err)
		assert.Empty(t, entries)
		listed, err := mgr.List(ctx)
		require.NoError(t, err)
		assert.Empty(t, listed, "a sandbox that never booted is not registered")
	})

	t.Run("exec_requires_guest_transport", func(t *testing.T) {
		mgr, _ := testManager(t, &fakeHypervisor{})
		info, err := mgr.Launch(ctx, vzLaunchOpts("MGIT-4.2"))
		require.NoError(t, err)
		_, err = mgr.Exec(ctx, info.ID, model.ExecRequest{Command: []string{"true"}})
		assert.Error(t, err, "exec is honestly unavailable until the mgit-guest transport (MGIT-11.5.6)")
		assert.NotErrorIs(t, err, model.ErrSandboxNotFound, "the sandbox itself exists")
	})

	t.Run("unknown_ids_not_found", func(t *testing.T) {
		mgr, _ := testManager(t, &fakeHypervisor{})
		_, err := mgr.Resolve(ctx, "01JXNOPE")
		assert.ErrorIs(t, err, model.ErrSandboxNotFound)
		assert.ErrorIs(t, mgr.Stop(ctx, "01JXNOPE", false), model.ErrSandboxNotFound)
		assert.ErrorIs(t, mgr.Remove(ctx, "01JXNOPE", false), model.ErrSandboxNotFound)
	})

	t.Run("stop_transitions_to_suspended", func(t *testing.T) {
		mgr, _ := testManager(t, &fakeHypervisor{})
		info, err := mgr.Launch(ctx, vzLaunchOpts("MGIT-4.2"))
		require.NoError(t, err)
		require.NoError(t, mgr.Stop(ctx, info.ID, false))
		resolved, err := mgr.Resolve(ctx, info.ID)
		require.NoError(t, err)
		assert.Equal(t, model.StateSuspended, resolved.State)
	})

	t.Run("graceful_stop_failure_surfaces_force_succeeds", func(t *testing.T) {
		hv := &fakeHypervisor{}
		mgr, _ := testManager(t, hv)
		info, err := mgr.Launch(ctx, vzLaunchOpts("MGIT-4.2"))
		require.NoError(t, err)
		hv.vms[0].failStop = true
		assert.Error(t, mgr.Stop(ctx, info.ID, false), "a stuck graceful stop surfaces")
		assert.Error(t, mgr.Remove(ctx, info.ID, false), "non-forced remove respects the stuck stop")
		assert.NoError(t, mgr.Remove(ctx, info.ID, true), "force removal always proceeds")
	})

	t.Run("exec_unknown_sandbox_not_found", func(t *testing.T) {
		mgr, _ := testManager(t, &fakeHypervisor{})
		_, err := mgr.Exec(ctx, "01JXNOPE", model.ExecRequest{Command: []string{"true"}})
		assert.ErrorIs(t, err, model.ErrSandboxNotFound)
	})

	t.Run("overlay_creation_failure_surfaces", func(t *testing.T) {
		hv := &fakeHypervisor{}
		mgr, workDir := testManager(t, hv)
		// Block the per-sandbox dir by squatting a FILE where the
		// manager needs a directory tree.
		blocked := filepath.Join(workDir, "blocked")
		require.NoError(t, os.WriteFile(blocked, nil, 0o600))
		_, err := createOverlay(filepath.Join(blocked, "x"), 0)
		assert.Error(t, err)
		_ = mgr
	})
}

// TestVZF_NewManager_Guards covers constructor validation.
func TestVZF_NewManager_Guards(t *testing.T) {
	images, _ := testImages(t)
	valid := Config{
		WorkDir:    t.TempDir(),
		Resolve:    func(string) (ImagePaths, error) { return images, nil },
		Hypervisor: &fakeHypervisor{},
		Logger:     slog.New(slog.NewTextHandler(os.Stderr, nil)),
		Clock:      time.Now,
	}
	_, err := NewManager(valid)
	require.NoError(t, err)

	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{name: "empty_work_dir", mutate: func(c *Config) { c.WorkDir = "" }},
		{name: "nil_resolver", mutate: func(c *Config) { c.Resolve = nil }},
		{name: "nil_logger", mutate: func(c *Config) { c.Logger = nil }},
		{name: "nil_clock", mutate: func(c *Config) { c.Clock = nil }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := valid
			tt.mutate(&cfg)
			_, err := NewManager(cfg)
			assert.Error(t, err)
		})
	}

	t.Run("unwritable_work_dir_rejected", func(t *testing.T) {
		cfg := valid
		blocker := filepath.Join(t.TempDir(), "blocker")
		require.NoError(t, os.WriteFile(blocker, nil, 0o600))
		cfg.WorkDir = filepath.Join(blocker, "nested")
		_, err := NewManager(cfg)
		assert.Error(t, err)
	})

	t.Run("image_digest_extraction", func(t *testing.T) {
		assert.Equal(t, "sha256:abc", imageDigest("img@sha256:abc"))
		assert.Empty(t, imageDigest("no-digest-here"))
	})
}
