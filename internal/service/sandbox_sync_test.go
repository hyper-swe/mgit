package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// syncingManager is a SandboxManager that also implements the OPTIONAL
// model.WorktreeSyncer capability, as the microVM backends do.
type syncingManager struct {
	fakeSandboxManager

	syncs      int
	lastSyncID string
	lastOpts   model.WorktreeSyncOptions
	report     *model.WorktreeSyncReport
	syncErr    error
}

func (m *syncingManager) SyncWorktree(_ context.Context, id string, opts model.WorktreeSyncOptions) (*model.WorktreeSyncReport, error) {
	m.syncs++
	m.lastSyncID, m.lastOpts = id, opts
	return m.report, m.syncErr
}

// bootedSync registers and boots a sandbox over a sync-capable manager.
func bootedSync(t *testing.T, mgr *syncingManager) *SandboxService {
	t.Helper()
	svc := newSvc(t, mgr, &fakeEventAppender{})
	_, err := svc.Register(context.Background(), regOpts("MGIT-76", "/work/a"))
	require.NoError(t, err)
	_, err = svc.EnsureRunning(context.Background(), "MGIT-76")
	require.NoError(t, err)
	return svc
}

// TestSandboxService_SyncWorktree_DelegatesToTheBackend verifies the service
// resolves task -> host-owned sandbox ID and passes the options through
// unchanged, so the CLI, MCP and the pre-exec path all mean the same thing by
// them. Refs: MGIT-76
func TestSandboxService_SyncWorktree_DelegatesToTheBackend(t *testing.T) {
	mgr := &syncingManager{report: &model.WorktreeSyncReport{Updated: []string{"app.go"}}}
	svc := bootedSync(t, mgr)

	got, err := svc.SyncWorktree(context.Background(), "MGIT-76", model.WorktreeSyncOptions{Force: true})

	require.NoError(t, err)
	assert.Equal(t, 1, mgr.syncs)
	assert.Equal(t, model.WorktreeSyncOptions{Force: true}, mgr.lastOpts)
	assert.NotEmpty(t, mgr.lastSyncID, "the backend is addressed by the host-owned sandbox ID, not the task")
	assert.Equal(t, []string{"app.go"}, got.Updated)
}

// TestSandboxService_SyncWorktree_DryRunReachesTheBackendUnchanged verifies the
// classification-only request is not quietly turned into a real sync anywhere
// in the service. Refs: MGIT-76
func TestSandboxService_SyncWorktree_DryRunReachesTheBackendUnchanged(t *testing.T) {
	mgr := &syncingManager{report: &model.WorktreeSyncReport{DryRun: true, Refused: true,
		Conflicts: []model.WorktreeSyncConflict{{Path: "app.go", Reason: "modified in the guest"}}}}
	svc := bootedSync(t, mgr)

	got, err := svc.SyncWorktree(context.Background(), "MGIT-76", model.WorktreeSyncOptions{DryRun: true})

	require.NoError(t, err)
	assert.True(t, mgr.lastOpts.DryRun)
	assert.True(t, got.DryRun)
	require.Len(t, got.Conflicts, 1)
	assert.Equal(t, "app.go", got.Conflicts[0].Path)
}

// TestSandboxService_SyncWorktree_ConflictCarriesBothErrorAndReport verifies a
// refusal reaches the caller as an error AND as the classification that caused
// it, so naming the diverged paths does not need a second round trip.
// Refs: MGIT-76
func TestSandboxService_SyncWorktree_ConflictCarriesBothErrorAndReport(t *testing.T) {
	mgr := &syncingManager{
		report:  &model.WorktreeSyncReport{Refused: true, Conflicts: []model.WorktreeSyncConflict{{Path: "app.go"}}},
		syncErr: model.ErrWorktreeSyncConflict,
	}
	svc := bootedSync(t, mgr)

	got, err := svc.SyncWorktree(context.Background(), "MGIT-76", model.WorktreeSyncOptions{})

	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrWorktreeSyncConflict)
	require.NotNil(t, got)
	assert.True(t, got.Refused)
	assert.Len(t, got.Conflicts, 1)
}

// TestSandboxService_SyncWorktree_UnregisteredTask_ReturnsNotFound verifies a
// task with no sandbox is rejected rather than silently reported as synced.
func TestSandboxService_SyncWorktree_UnregisteredTask_ReturnsNotFound(t *testing.T) {
	mgr := &syncingManager{}
	svc := newSvc(t, mgr, &fakeEventAppender{})

	_, err := svc.SyncWorktree(context.Background(), "MGIT-99", model.WorktreeSyncOptions{})

	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrSandboxNotFound)
	assert.Zero(t, mgr.syncs)
}

// TestSandboxService_SyncWorktree_RegisteredButNotBooted_IsAnHonestNoOp
// verifies the lazy-provisioning case: a sandbox that has not booted has no
// guest tree, and it will stage the CURRENT worktree when it does. That is a
// genuine nothing-to-do, reported with the reason — and crucially it does NOT
// boot a VM as a side effect of asking. Refs: FR-17.10, MGIT-76
func TestSandboxService_SyncWorktree_RegisteredButNotBooted_IsAnHonestNoOp(t *testing.T) {
	mgr := &syncingManager{}
	svc := newSvc(t, mgr, &fakeEventAppender{})
	_, err := svc.Register(context.Background(), regOpts("MGIT-76", "/work/a"))
	require.NoError(t, err)

	got, err := svc.SyncWorktree(context.Background(), "MGIT-76", model.WorktreeSyncOptions{})

	require.NoError(t, err)
	assert.True(t, got.Skipped)
	assert.Contains(t, got.Detail, "boot")
	assert.Zero(t, mgr.launches, "asking to sync must never boot a VM")
	assert.Zero(t, mgr.syncs)
}

// TestSandboxService_SyncWorktree_BackendWithoutTheCapability_FailsClosed
// verifies a manager that cannot sync is reported as such rather than being
// treated as a successful no-op — the firecracker case, seen from the service.
// Refs: MGIT-76, ADR-011
func TestSandboxService_SyncWorktree_BackendWithoutTheCapability_FailsClosed(t *testing.T) {
	mgr := &fakeSandboxManager{} // no SyncWorktree method
	svc := newSvc(t, mgr, &fakeEventAppender{})
	_, err := svc.Register(context.Background(), regOpts("MGIT-76", "/work/a"))
	require.NoError(t, err)
	_, err = svc.EnsureRunning(context.Background(), "MGIT-76")
	require.NoError(t, err)

	got, err := svc.SyncWorktree(context.Background(), "MGIT-76", model.WorktreeSyncOptions{})

	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrSandboxSyncUnsupported)
	assert.Nil(t, got)
}
