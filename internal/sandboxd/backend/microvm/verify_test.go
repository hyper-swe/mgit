package microvm

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// Asking a sandbox whether its guest reads what was last delivered: a guest
// that still reads old bytes is reported path by path, a guest that agrees
// is reported with the count it confirmed, and a sandbox nothing was ever
// delivered to says so instead of passing on an empty comparison.
// Refs: MGIT-164, MGIT-192
func TestManager_VerifyGuestView_ReportsWhatTheGuestReadsAgainstTheLastDelivery(t *testing.T) {
	f := newSyncFixture(t, map[string]string{"app.go": "V1", "lib.go": "L"})
	f.mgr.settler = &fakeSettler{}
	writeFiles(t, f.worktree, map[string]string{"app.go": "V2"})
	_, err := f.mgr.SyncWorktree(context.Background(), f.id, model.WorktreeSyncOptions{})
	require.NoError(t, err)

	settled := &fakeSettler{}
	f.mgr.settler = settled
	report, err := f.mgr.VerifyGuestView(context.Background(), f.id)
	require.NoError(t, err)
	assert.Empty(t, report.Stale)
	assert.Equal(t, 2, report.Checked, "every path in the delivered manifest is asked about, not just the last sync's")
	assert.Empty(t, report.Unverifiable)
	require.Len(t, settled.calls, 1)
	assert.Contains(t, settled.calls[0].want, "lib.go", "the comparison is against the whole delivered manifest")

	f.mgr.settler = &fakeSettler{never: true}
	report, err = f.mgr.VerifyGuestView(context.Background(), f.id)
	require.NoError(t, err)
	assert.Len(t, report.Stale, 2, "a guest that reads old bytes is reported path by path, at once — no waiting: doctor asks, it does not fix")
	assert.Contains(t, report.Stale[0], ".go")
}

// The launch-time staging is itself a delivery, so a booted sandbox always
// has a manifest; the "nothing delivered" answer belongs to a sandbox whose
// record of deliveries is missing — and it is an honest "cannot tell", not
// a pass on an empty comparison. Refs: MGIT-164, R-H300
func TestManager_VerifyGuestView_NoDeliveryRecord_IsUnverifiableNotAPass(t *testing.T) {
	f := newSyncFixture(t, map[string]string{"app.go": "V1"})
	f.mgr.settler = &fakeSettler{}
	require.NoError(t, os.Remove(filepath.Join(filepath.Dir(f.staged), "sync-manifest.json")))

	report, err := f.mgr.VerifyGuestView(context.Background(), f.id)

	require.NoError(t, err)
	assert.Equal(t, 0, report.Checked)
	assert.Contains(t, report.Unverifiable, "nothing has been delivered")
}

func TestManager_VerifyGuestView_UnknownSandbox_ReturnsNotFound(t *testing.T) {
	f := newSyncFixture(t, map[string]string{"app.go": "V1"})
	_, err := f.mgr.VerifyGuestView(context.Background(), "01NOPE")
	assert.ErrorIs(t, err, model.ErrSandboxNotFound)
}
