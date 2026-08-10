package sandboxd

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// TestClient_SyncWorktree_RoundTrip verifies the verb completes a full
// request/response against a live daemon with its options intact. Refs: MGIT-76
func TestClient_SyncWorktree_RoundTrip(t *testing.T) {
	svc := &fakeDispatcher{syncReport: &model.WorktreeSyncReport{
		Updated: []string{"app.go"}, Overridden: []string{"lib.go"},
	}}
	client, stop := newClientForDaemon(t, svc)
	defer stop()

	got, err := client.SyncWorktree(context.Background(), "MGIT-76", model.WorktreeSyncOptions{Force: true})

	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, []string{"app.go"}, got.Updated)
	assert.Equal(t, []string{"lib.go"}, got.Overridden)
	assert.Equal(t, "MGIT-76", svc.syncTask)
	assert.True(t, svc.syncOpts.Force)
}

// TestClient_SyncWorktree_Refusal_ReturnsErrorAndReport verifies the client
// surfaces a refusal as an error WITHOUT discarding the classification — the
// ordinary round trip drops the response body on error, and doing that here
// would throw away the conflict report this verb exists to deliver.
// Refs: MGIT-76
func TestClient_SyncWorktree_Refusal_ReturnsErrorAndReport(t *testing.T) {
	svc := &fakeDispatcher{
		syncReport: &model.WorktreeSyncReport{Refused: true,
			Conflicts: []model.WorktreeSyncConflict{{Path: "app.go", Reason: "modified in the guest"}}},
		syncErr: model.ErrWorktreeSyncConflict,
	}
	client, stop := newClientForDaemon(t, svc)
	defer stop()

	got, err := client.SyncWorktree(context.Background(), "MGIT-76", model.WorktreeSyncOptions{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "blocked by guest-side changes")
	require.NotNil(t, got, "the refusal must still carry what it refused")
	assert.True(t, got.Refused)
	require.Len(t, got.Conflicts, 1)
	assert.Equal(t, "app.go", got.Conflicts[0].Path)
}

// TestClient_SyncWorktree_NoDaemon_FailsClosed verifies an unreachable daemon
// is an error, never an empty report a caller could read as "nothing to do".
func TestClient_SyncWorktree_NoDaemon_FailsClosed(t *testing.T) {
	client := NewClient(shortSocketPath(t), time.Now) // never served

	got, err := client.SyncWorktree(context.Background(), "MGIT-76", model.WorktreeSyncOptions{})

	require.Error(t, err)
	assert.Nil(t, got)
}
