package microvm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/provision"
	"github.com/hyper-swe/mgit/internal/sandboxd/staging"
)

// stagingHypervisor stands in for a virtiofs backend (libkrun, vzf): it builds
// the quarantined staging tree the guest is shown, in the same place and by
// the same code the real ones use, so the manager's sync path is exercised
// against a realistic delivery. Refs: ADR-011
type stagingHypervisor struct{ fakeHypervisor }

func (h *stagingHypervisor) CreateVM(cfg VMConfig) (VM, error) {
	vm, err := h.fakeHypervisor.CreateVM(cfg)
	if err != nil {
		return nil, err
	}
	if cfg.WorktreePath != "" && cfg.PrivateStorePath != "" {
		if berr := staging.Build(cfg.WorktreePath, cfg.PrivateStorePath,
			filepath.Join(cfg.StateDir, stagedTreeName)); berr != nil {
			return nil, berr
		}
	}
	return vm, nil
}

// dirProvisioner is a minimal SEC-03 private store: a real directory outside
// the worktree, which is all the manager and the staging build need of it here.
type dirProvisioner struct{ sharedDir string }

func (p dirProvisioner) Provision(_, privateDir string) (provision.PrivateStore, error) {
	if err := os.MkdirAll(privateDir, 0o750); err != nil {
		return provision.PrivateStore{}, err
	}
	if err := os.WriteFile(filepath.Join(privateDir, "HEAD"), []byte("ref: refs/heads/task"), 0o600); err != nil {
		return provision.PrivateStore{}, err
	}
	return provision.PrivateStore{Dir: privateDir, SharedDir: p.sharedDir}, nil
}

// syncFixture is one launched sandbox delivering a real worktree through a
// staged copy: the layout every sync assertion below runs against.
type syncFixture struct {
	mgr      *Manager
	id       string
	worktree string
	staged   string
}

// newSyncFixture launches a sandbox over a virtiofs-style backend and returns
// the live worktree plus the staged tree the guest sees.
func newSyncFixture(t *testing.T, files map[string]string) syncFixture {
	t.Helper()
	project := t.TempDir()
	worktree := filepath.Join(project, "wt")
	require.NoError(t, os.MkdirAll(filepath.Join(project, ".mgit"), 0o750))
	require.NoError(t, os.MkdirAll(worktree, 0o750))
	writeFiles(t, worktree, files)

	hv := &stagingHypervisor{}
	mgr, workDir := testManager(t, hv)
	mgr.cfg.StoreProvisioner = dirProvisioner{sharedDir: filepath.Join(project, ".mgit")}

	opts := launchOpts("MGIT-76", model.NetworkModeNone)
	opts.WorktreePath = worktree
	info, err := mgr.Launch(context.Background(), opts)
	require.NoError(t, err)

	return syncFixture{
		mgr: mgr, id: info.ID, worktree: worktree,
		staged: filepath.Join(SandboxStateDir(workDir, info.ID), stagedTreeName),
	}
}

// writeFiles writes a path->content map under root, creating parents.
func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		full := filepath.Join(root, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o750))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o600))
	}
}

// readStaged reads one path out of the tree the guest sees.
func readStaged(t *testing.T, f syncFixture, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.staged, rel)) //nolint:gosec // test-owned tree
	require.NoError(t, err)
	return string(data)
}

// TestManager_SyncWorktree_HostEdit_ReachesTheGuestTree is the POSITIVE
// CONTROL for every refusal below: the explicit verb really does deliver, so a
// refusal elsewhere is distinguishable from a broken path. Refs: MGIT-76
func TestManager_SyncWorktree_HostEdit_ReachesTheGuestTree(t *testing.T) {
	f := newSyncFixture(t, map[string]string{"app.go": "V1"})
	require.Equal(t, "V1", readStaged(t, f, "app.go"))

	writeFiles(t, f.worktree, map[string]string{"app.go": "V2"})
	res, err := f.mgr.SyncWorktree(context.Background(), f.id, model.WorktreeSyncOptions{})

	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Equal(t, []string{"app.go"}, res.Updated)
	assert.False(t, res.Skipped)
	assert.False(t, res.DryRun)
	assert.Equal(t, "V2", readStaged(t, f, "app.go"),
		"the guest's tree must carry the NEW content, not merely lose the old")
}

// TestManager_SyncWorktree_UnchangedWorktree_IsAGenuineNoOp verifies an
// unchanged worktree SAYS so rather than reporting phantom work. Refs: MGIT-76
func TestManager_SyncWorktree_UnchangedWorktree_IsAGenuineNoOp(t *testing.T) {
	f := newSyncFixture(t, map[string]string{"app.go": "V1"})

	res, err := f.mgr.SyncWorktree(context.Background(), f.id, model.WorktreeSyncOptions{})

	require.NoError(t, err)
	assert.True(t, res.Skipped)
	assert.False(t, res.Changed())
	assert.Empty(t, res.Updated)
	assert.Empty(t, res.Deleted)
}

// TestManager_SyncWorktree_DryRun_ClassifiesWithoutTouchingTheGuest is the
// report HyperSwe cannot get today without attempting work in the guest.
// Refs: MGIT-76
func TestManager_SyncWorktree_DryRun_ClassifiesWithoutTouchingTheGuest(t *testing.T) {
	f := newSyncFixture(t, map[string]string{"app.go": "V1"})
	writeFiles(t, f.worktree, map[string]string{"app.go": "V2", "new.go": "NEW"})

	res, err := f.mgr.SyncWorktree(context.Background(), f.id, model.WorktreeSyncOptions{DryRun: true})

	require.NoError(t, err)
	assert.True(t, res.DryRun)
	assert.Equal(t, []string{"app.go", "new.go"}, res.Updated)
	assert.False(t, res.Refused)
	assert.Equal(t, "V1", readStaged(t, f, "app.go"), "a dry run delivers nothing")
	assert.NoFileExists(t, filepath.Join(f.staged, "new.go"))
}

// TestManager_SyncWorktree_DryRun_ReportsConflictsWithoutAnExec is the
// capability the ticket exists for: the conflict classification, obtained
// without running anything in the guest and without being refused.
// Refs: MGIT-76
func TestManager_SyncWorktree_DryRun_ReportsConflictsWithoutAnExec(t *testing.T) {
	f := newSyncFixture(t, map[string]string{"app.go": "V1"})
	writeFiles(t, f.staged, map[string]string{"app.go": "GUEST-WIP"}) // the guest edited it
	writeFiles(t, f.worktree, map[string]string{"app.go": "V2"})

	res, err := f.mgr.SyncWorktree(context.Background(), f.id, model.WorktreeSyncOptions{DryRun: true})

	require.NoError(t, err, "a dry run reports; it does not refuse")
	assert.True(t, res.Refused, "the report must say a real sync would be refused")
	require.Len(t, res.Conflicts, 1)
	assert.Equal(t, "app.go", res.Conflicts[0].Path)
	assert.Contains(t, res.Conflicts[0].Reason, "modified in the guest")
	assert.Equal(t, "GUEST-WIP", readStaged(t, f, "app.go"))
}

// TestManager_SyncWorktree_Conflict_IsRefusedNamingEveryPath verifies the
// refusal carries something a caller can act on. Refs: MGIT-71, MGIT-76
func TestManager_SyncWorktree_Conflict_IsRefusedNamingEveryPath(t *testing.T) {
	f := newSyncFixture(t, map[string]string{"app.go": "V1", "lib.go": "L1"})
	writeFiles(t, f.staged, map[string]string{"app.go": "GUEST-WIP", "lib.go": "GUEST-LIB"})
	writeFiles(t, f.worktree, map[string]string{"app.go": "V2", "lib.go": "L2"})

	res, err := f.mgr.SyncWorktree(context.Background(), f.id, model.WorktreeSyncOptions{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "app.go")
	assert.Contains(t, err.Error(), "lib.go")
	require.NotNil(t, res, "a refusal must still carry the classification")
	assert.True(t, res.Refused)
	assert.Len(t, res.Conflicts, 2)
	assert.Equal(t, "GUEST-WIP", readStaged(t, f, "app.go"), "a refused sync applies nothing")
}

// TestManager_SyncWorktree_Force_OverwritesAndReportsEveryDestroyedPath
// verifies --force matches the pre-exec semantics: it overwrites, and every
// destroyed path is reported so it reaches the audit record. Refs: MGIT-76
func TestManager_SyncWorktree_Force_OverwritesAndReportsEveryDestroyedPath(t *testing.T) {
	f := newSyncFixture(t, map[string]string{"app.go": "V1"})
	writeFiles(t, f.staged, map[string]string{"app.go": "GUEST-WIP"})
	writeFiles(t, f.worktree, map[string]string{"app.go": "V2"})

	res, err := f.mgr.SyncWorktree(context.Background(), f.id, model.WorktreeSyncOptions{Force: true})

	require.NoError(t, err)
	assert.Equal(t, []string{"app.go"}, res.Overridden)
	assert.Equal(t, "V2", readStaged(t, f, "app.go"))
}

// TestManager_SyncWorktree_GuestCreatedPaths_AreNeverTouched verifies the third
// collision class survives the explicit verb exactly as it survives the
// automatic one. Refs: MGIT-71, MGIT-76
func TestManager_SyncWorktree_GuestCreatedPaths_AreNeverTouched(t *testing.T) {
	f := newSyncFixture(t, map[string]string{"app.go": "V1"})
	writeFiles(t, f.staged, map[string]string{"node_modules/dep/index.js": "CACHED"})
	writeFiles(t, f.worktree, map[string]string{"app.go": "V2"})

	_, err := f.mgr.SyncWorktree(context.Background(), f.id, model.WorktreeSyncOptions{})

	require.NoError(t, err)
	assert.Equal(t, "CACHED", readStaged(t, f, "node_modules/dep/index.js"))
}

// TestManager_SyncWorktree_EscapingSymlink_FailsClosedLikeALaunch is the
// single-path invariant: the explicit verb cannot deliver anything a launch or
// a pre-exec stage would have refused. Refs: SEC-03, MGIT-76
func TestManager_SyncWorktree_EscapingSymlink_FailsClosedLikeALaunch(t *testing.T) {
	f := newSyncFixture(t, map[string]string{"app.go": "V1"})
	outside := filepath.Join(t.TempDir(), "host-secret")
	require.NoError(t, os.WriteFile(outside, []byte("host secret"), 0o600))
	require.NoError(t, os.Symlink(outside, filepath.Join(f.worktree, "escape")))

	_, err := f.mgr.SyncWorktree(context.Background(), f.id, model.WorktreeSyncOptions{})

	require.Error(t, err)
	assert.ErrorIs(t, err, staging.ErrSymlinkEscape)
	assert.NoFileExists(t, filepath.Join(f.staged, "escape"))
}

// TestManager_SyncWorktree_ImageBackedWorktree_FailsClosedNamingTheLimitation
// is the firecracker case: the worktree is an ext4 image built at launch, so
// there is no staged directory to write into. It must NOT silently no-op and
// report success — a sync that claims to have run is how stale code gets
// executed. Refs: MGIT-76, ADR-011
func TestManager_SyncWorktree_ImageBackedWorktree_FailsClosedNamingTheLimitation(t *testing.T) {
	// The plain fake hypervisor leaves no staged tree behind, exactly as
	// firecracker does not (its staging dir is consumed by mke2fs and removed).
	hv := &fakeHypervisor{}
	mgr, _ := testManager(t, hv)
	info, err := mgr.Launch(context.Background(), launchOpts("MGIT-76", model.NetworkModeNone))
	require.NoError(t, err)

	for _, opts := range []model.WorktreeSyncOptions{{}, {DryRun: true}, {Force: true}} {
		res, syncErr := mgr.SyncWorktree(context.Background(), info.ID, opts)

		require.Error(t, syncErr, "opts=%+v must not report success", opts)
		assert.ErrorIs(t, syncErr, model.ErrSandboxSyncUnsupported)
		assert.Contains(t, syncErr.Error(), model.BackendKVM, "the refusal names the backend")
		assert.Contains(t, syncErr.Error(), "re-launch", "the refusal names the remedy")
		assert.Nil(t, res)
	}
}

// TestManager_SyncWorktree_UnknownSandbox_ReturnsNotFound verifies an
// unregistered ID is rejected rather than treated as an empty tree.
func TestManager_SyncWorktree_UnknownSandbox_ReturnsNotFound(t *testing.T) {
	mgr, _ := testManager(t, &stagingHypervisor{})

	_, err := mgr.SyncWorktree(context.Background(), "no-such-sandbox", model.WorktreeSyncOptions{})

	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrSandboxNotFound)
}

// TestManager_SyncWorktree_SatisfiesTheModelContract pins the optional backend
// capability to its interface, so the service can discover it by type
// assertion without importing this package. Refs: MGIT-76
func TestManager_SyncWorktree_SatisfiesTheModelContract(t *testing.T) {
	mgr, _ := testManager(t, &stagingHypervisor{})

	var syncer model.WorktreeSyncer = mgr

	assert.NotNil(t, syncer)
}

// TestManager_SyncBeforeExec_SharesTheSyncPath verifies the automatic pre-exec
// stage and the explicit verb are the SAME mechanism: a host edit is delivered
// by exec, and the sync that follows then reports nothing left to do.
// Refs: MGIT-71, MGIT-76
func TestManager_SyncBeforeExec_SharesTheSyncPath(t *testing.T) {
	f := newSyncFixture(t, map[string]string{"app.go": "V1"})
	writeFiles(t, f.worktree, map[string]string{"app.go": "V2"})

	f.mgr.mu.Lock()
	sb := f.mgr.sandboxes[f.id]
	f.mgr.mu.Unlock()
	require.NoError(t, f.mgr.syncBeforeExec(context.Background(), sb))

	assert.Equal(t, "V2", readStaged(t, f, "app.go"))
	res, err := f.mgr.SyncWorktree(context.Background(), f.id, model.WorktreeSyncOptions{})
	require.NoError(t, err)
	assert.True(t, res.Skipped, "the explicit verb sees the pre-exec stage's baseline, not its own")
}

// TestManager_SyncBeforeExec_Conflict_StillRefusesTheExec verifies the
// automatic path's fail-closed behavior is unchanged by the new verb.
// Refs: MGIT-71
func TestManager_SyncBeforeExec_Conflict_StillRefusesTheExec(t *testing.T) {
	f := newSyncFixture(t, map[string]string{"app.go": "V1"})
	writeFiles(t, f.staged, map[string]string{"app.go": "GUEST-WIP"})
	writeFiles(t, f.worktree, map[string]string{"app.go": "V2"})

	f.mgr.mu.Lock()
	sb := f.mgr.sandboxes[f.id]
	f.mgr.mu.Unlock()
	err := f.mgr.syncBeforeExec(context.Background(), sb)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "exec refused")
	assert.Contains(t, err.Error(), "app.go")
	assert.True(t, errors.Is(err, model.ErrWorktreeSyncConflict),
		"the refusal must be classifiable, not only readable: %v", err)
}
