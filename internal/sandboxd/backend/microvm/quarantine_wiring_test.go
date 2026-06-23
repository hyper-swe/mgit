package microvm

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/provision"
)

// fakeProvisioner records the provision request and returns a private store
// rooted at a controllable shared dir, so the manager's quarantine bind +
// fail-closed wiring can be exercised without a real go-git store.
type fakeProvisioner struct {
	sharedDir   string // the SharedDir the BindPrivateStore check sees
	err         error  // a provisioning failure (fails the launch)
	gotTask     string
	gotPrivDir  string
	calls       int
	makePrivDir bool // create the private dir on disk (default true)
}

func (f *fakeProvisioner) Provision(taskID, privateDir string) (provision.PrivateStore, error) {
	f.calls++
	f.gotTask = taskID
	f.gotPrivDir = privateDir
	if f.err != nil {
		return provision.PrivateStore{}, f.err
	}
	if f.makePrivDir {
		_ = os.MkdirAll(privateDir, 0o700)
	}
	return provision.PrivateStore{Dir: privateDir, SharedDir: f.sharedDir}, nil
}

func quarantineManager(t *testing.T, hv Hypervisor, prov provision.Provisioner) *Manager {
	t.Helper()
	images := testImages(t)
	mgr, err := NewManager(Config{
		Backend:          model.BackendKVM,
		WorkDir:          t.TempDir(),
		Resolve:          func(string) (ImagePaths, error) { return images, nil },
		Hypervisor:       hv,
		StoreProvisioner: prov,
		SensitivePaths:   []string{".claude/", ".git/hooks/"},
		Logger:           slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		Clock:            func() time.Time { return time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC) },
	})
	require.NoError(t, err)
	return mgr
}

// TestManager_Launch_WithProvisioner_BindsPrivateStore proves the SEC-03 wiring:
// the manager provisions a private store per launch, the VM config carries its
// path, and the shared store (a sibling of the worktree) is bound without
// error. Refs: SEC-03
func TestManager_Launch_WithProvisioner_BindsPrivateStore(t *testing.T) {
	// A worktree with the shared store as a sibling (the real layout): bind
	// succeeds because the shared store is outside the worktree.
	base := t.TempDir()
	worktree := filepath.Join(base, "worktrees", "MGIT-11.6.8")
	require.NoError(t, os.MkdirAll(worktree, 0o750))
	shared := filepath.Join(base, ".mgit")
	require.NoError(t, os.MkdirAll(shared, 0o750))

	prov := &fakeProvisioner{sharedDir: shared, makePrivDir: true}
	hv := &fakeHypervisor{}
	mgr := quarantineManager(t, hv, prov)

	opts := launchOpts("MGIT-11.6.8", model.NetworkModeNone)
	opts.WorktreePath = worktree
	info, err := mgr.Launch(context.Background(), opts)
	require.NoError(t, err)

	require.Equal(t, 1, prov.calls, "the launch provisions exactly one private store")
	assert.Equal(t, "MGIT-11.6.8", prov.gotTask)
	require.Len(t, hv.configs, 1)
	cfg := hv.configs[0]
	assert.NotEmpty(t, cfg.PrivateStorePath, "the VM config carries the private store path (SEC-03)")
	assert.Equal(t, prov.gotPrivDir, cfg.PrivateStorePath)
	assert.True(t, strings.HasPrefix(cfg.PrivateStorePath, mgr.cfg.WorkDir),
		"the private store lives in the sandbox state dir, outside the worktree")
	assert.Equal(t, worktree, cfg.WorktreePath, "the worktree is still shared at its identical path")
	assert.Equal(t, model.StateRunning, info.State)
}

// TestManager_Launch_SharedStoreReachable_FailsClosed proves a leaky layout
// (the shared store INSIDE the worktree) is rejected: BindPrivateStore returns
// ErrSharedStoreReachable, the launch fails, and no VM is created. Refs: SEC-03
func TestManager_Launch_SharedStoreReachable_FailsClosed(t *testing.T) {
	base := t.TempDir()
	worktree := filepath.Join(base, "worktrees", "MGIT-11.6.8")
	require.NoError(t, os.MkdirAll(worktree, 0o750))
	// Leaky: the shared store resolves INSIDE the mounted worktree.
	sharedInsideWorktree := filepath.Join(worktree, ".mgit")

	prov := &fakeProvisioner{sharedDir: sharedInsideWorktree, makePrivDir: true}
	hv := &fakeHypervisor{}
	mgr := quarantineManager(t, hv, prov)

	opts := launchOpts("MGIT-11.6.8", model.NetworkModeNone)
	opts.WorktreePath = worktree
	_, err := mgr.Launch(context.Background(), opts)
	require.Error(t, err)
	assert.True(t, errors.Is(err, model.ErrSharedStoreReachable),
		"a shared store reachable from the guest fails the launch closed")
	assert.Empty(t, hv.configs, "no VM is created when quarantine fails")
}

// TestManager_Launch_ProvisionFailure_FailsClosed proves a provisioning error
// (e.g. the task base branch is missing) fails the launch with no VM. Refs: SEC-03
func TestManager_Launch_ProvisionFailure_FailsClosed(t *testing.T) {
	worktree := t.TempDir()
	prov := &fakeProvisioner{err: model.ErrBranchNotFound}
	hv := &fakeHypervisor{}
	mgr := quarantineManager(t, hv, prov)

	opts := launchOpts("MGIT-11.6.8", model.NetworkModeNone)
	opts.WorktreePath = worktree
	_, err := mgr.Launch(context.Background(), opts)
	require.Error(t, err)
	assert.True(t, errors.Is(err, model.ErrBranchNotFound))
	assert.Empty(t, hv.configs, "no VM is created when provisioning fails")
}

// TestManager_Launch_NoProvisioner_NoPrivateStore proves the legacy/direct path:
// with no provisioner the quarantine step is a no-op and PrivateStorePath stays
// empty (the pre-SEC-03 delivery). Refs: SEC-03
func TestManager_Launch_NoProvisioner_NoPrivateStore(t *testing.T) {
	hv := &fakeHypervisor{}
	mgr := quarantineManager(t, hv, nil)
	_, err := mgr.Launch(context.Background(), launchOpts("MGIT-9.9", model.NetworkModeNone))
	require.NoError(t, err)
	require.Len(t, hv.configs, 1)
	assert.Empty(t, hv.configs[0].PrivateStorePath, "no provisioner: no private store rebind")
}
