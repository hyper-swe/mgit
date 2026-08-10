package sandboxd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/controlproto"
	"github.com/hyper-swe/mgit/internal/model"
)

// syncRoundTrip serves one sync request against a daemon wired with svc.
func syncRoundTrip(t *testing.T, svc SandboxDispatcher, args *controlproto.SyncArgs) *controlproto.Response {
	t.Helper()
	skipUnsupportedHostIPC(t)
	cfg, _ := dispatchConfig(t, svc)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = runDaemon(ctx, t, cfg)

	conn := dialAuthed(t, cfg.SocketPath)
	defer func() { _ = conn.Close() }()

	require.NoError(t, controlproto.WriteRequest(conn, &controlproto.Request{Kind: controlproto.KindSync, Sync: args}))
	resp, err := controlproto.ReadResponse(conn)
	require.NoError(t, err)
	return resp
}

// TestDaemon_Sync_RoutesThroughTheServiceAndReturnsTheReport verifies the new
// verb is served through the service (never the manager) and the whole
// classification reaches the client. Refs: MGIT-76, FR-17.16
func TestDaemon_Sync_RoutesThroughTheServiceAndReturnsTheReport(t *testing.T) {
	svc := &fakeDispatcher{syncReport: &model.WorktreeSyncReport{Updated: []string{"app.go"}, Deleted: []string{"old.go"}}}

	resp := syncRoundTrip(t, svc, &controlproto.SyncArgs{
		TaskID: "MGIT-76", Sync: model.WorktreeSyncOptions{DryRun: true},
	})

	assert.Empty(t, resp.Error)
	require.NotNil(t, resp.Synced)
	assert.Equal(t, []string{"app.go"}, resp.Synced.Updated)
	assert.Equal(t, []string{"old.go"}, resp.Synced.Deleted)
	assert.Equal(t, "MGIT-76", svc.syncTask)
	assert.True(t, svc.syncOpts.DryRun, "the options must reach the service unchanged")
}

// TestDaemon_Sync_Refusal_CarriesBothTheErrorAndTheConflicts verifies a
// refusal is reported as an error AND names the diverged paths, so the caller
// does not have to run a second query to find out what blocked it.
// Refs: MGIT-76
func TestDaemon_Sync_Refusal_CarriesBothTheErrorAndTheConflicts(t *testing.T) {
	svc := &fakeDispatcher{
		syncReport: &model.WorktreeSyncReport{Refused: true,
			Conflicts: []model.WorktreeSyncConflict{{Path: "app.go", Reason: "modified in the guest"}}},
		syncErr: model.ErrWorktreeSyncConflict,
	}

	resp := syncRoundTrip(t, svc, &controlproto.SyncArgs{TaskID: "MGIT-76"})

	assert.NotEmpty(t, resp.Error, "a refusal must remain an error")
	require.NotNil(t, resp.Synced, "a refusal must still name what it refused")
	assert.True(t, resp.Synced.Refused)
	require.Len(t, resp.Synced.Conflicts, 1)
	assert.Equal(t, "app.go", resp.Synced.Conflicts[0].Path)
}
