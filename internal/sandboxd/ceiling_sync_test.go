package sandboxd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// syncingInner is a backend that carries the OPTIONAL worktree-sync
// capability, as the microVM manager does.
type syncingInner struct {
	launchingManager

	syncs    int
	lastID   string
	lastOpts model.WorktreeSyncOptions
	report   *model.WorktreeSyncReport
	err      error
}

func (m *syncingInner) SyncWorktree(_ context.Context, id string, opts model.WorktreeSyncOptions) (*model.WorktreeSyncReport, error) {
	m.syncs++
	m.lastID, m.lastOpts = id, opts
	return m.report, m.err
}

// TestCeilingManager_SyncWorktree_ForwardsToACapableBackend is the decorator
// trap: the ceiling wraps every backend, so a capability it does not forward
// disappears entirely and the service sees an unsyncable sandbox. That failure
// is invisible in the backend's own tests. Refs: MGIT-76
func TestCeilingManager_SyncWorktree_ForwardsToACapableBackend(t *testing.T) {
	inner := &syncingInner{launchingManager: launchingManager{fakeManager: *newFakeManager()},
		report: &model.WorktreeSyncReport{Updated: []string{"app.go"}}}
	mgr := NewCeilingManager(inner, 0, 0, 0)

	var syncer model.WorktreeSyncer = mgr
	got, err := syncer.SyncWorktree(context.Background(), "sbx-1", model.WorktreeSyncOptions{DryRun: true})

	require.NoError(t, err)
	assert.Equal(t, 1, inner.syncs)
	assert.Equal(t, "sbx-1", inner.lastID)
	assert.True(t, inner.lastOpts.DryRun, "the options must cross the decorator unchanged")
	assert.Equal(t, []string{"app.go"}, got.Updated)
}

// TestCeilingManager_SyncWorktree_IncapableBackend_FailsClosed verifies the
// decorator does not INVENT the capability either: a backend that cannot sync
// must still say so, or the ceiling would turn firecracker's real limitation
// into a silent success. Refs: MGIT-76, ADR-011
func TestCeilingManager_SyncWorktree_IncapableBackend_FailsClosed(t *testing.T) {
	inner := &launchingManager{fakeManager: *newFakeManager()}
	mgr := NewCeilingManager(inner, 0, 0, 0)

	got, err := mgr.SyncWorktree(context.Background(), "sbx-1", model.WorktreeSyncOptions{})

	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrSandboxSyncUnsupported)
	assert.Nil(t, got)
}
