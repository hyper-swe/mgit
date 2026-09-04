package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// verifyingManager is a sync-capable manager that can also say what its
// guest reads.
type verifyingManager struct {
	syncingManager
	verifies int
	lastID   string
	view     *model.GuestViewReport
}

func (m *verifyingManager) VerifyGuestView(_ context.Context, id string) (*model.GuestViewReport, error) {
	m.verifies++
	m.lastID = id
	return m.view, nil
}

// The service resolves the task to the host-owned sandbox ID and hands the
// question to the backend; the answer comes back verbatim. Refs: MGIT-164
func TestSandboxService_VerifyGuestView_DelegatesToTheBackend(t *testing.T) {
	mgr := &verifyingManager{view: &model.GuestViewReport{Checked: 4, Stale: []string{"app.go (guest reads the old bytes)"}}}
	svc := bootedSync(t, &mgr.syncingManager)
	svc.manager = mgr

	got, err := svc.VerifyGuestView(context.Background(), "MGIT-76")

	require.NoError(t, err)
	assert.Equal(t, 1, mgr.verifies)
	assert.NotEmpty(t, mgr.lastID, "the backend is addressed by the sandbox ID, not the task")
	assert.Equal(t, 4, got.Checked)
	assert.Equal(t, []string{"app.go (guest reads the old bytes)"}, got.Stale)
}

// A sandbox that has not booted has had nothing delivered: the answer says
// so, and asking must never boot a VM. Refs: MGIT-164, FR-17.9
func TestSandboxService_VerifyGuestView_RegisteredButNotBooted_SaysNothingWasDelivered(t *testing.T) {
	mgr := &verifyingManager{}
	svc := newSvc(t, &mgr.syncingManager, &fakeEventAppender{})
	svc.manager = mgr
	_, err := svc.Register(context.Background(), regOpts("MGIT-76", "/work/a"))
	require.NoError(t, err)

	got, err := svc.VerifyGuestView(context.Background(), "MGIT-76")

	require.NoError(t, err)
	assert.Contains(t, got.Unverifiable, "nothing has been delivered")
	assert.Zero(t, got.Checked)
	assert.Zero(t, mgr.launches, "asking must never boot a VM")
	assert.Zero(t, mgr.verifies)
}

// A backend that delivers the worktree as a launch-time copy has no later
// delivery to compare against; that is reported, not passed. Refs: MGIT-164
func TestSandboxService_VerifyGuestView_BackendWithoutTheCapability_FailsClosed(t *testing.T) {
	mgr := &syncingManager{} // syncs, but cannot say what the guest reads
	svc := bootedSync(t, mgr)

	_, err := svc.VerifyGuestView(context.Background(), "MGIT-76")

	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrSandboxSyncUnsupported)
	assert.Contains(t, err.Error(), "launch-time")
}
