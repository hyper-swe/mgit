package sandboxd

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/controlproto"
	"github.com/hyper-swe/mgit/internal/model"
)

// fakeGrantCoordinator records the resolved sandbox ID it was asked about and
// returns canned pending requests / grant outcomes.
type fakeGrantCoordinator struct {
	mu sync.Mutex

	pendingFor string
	approveSbx string
	approveKey string
	pending    []model.CapabilityRequest
	grant      *model.CapabilityGrant
	approveErr error
}

func (f *fakeGrantCoordinator) PendingRequests(sandboxID string) []model.CapabilityRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pendingFor = sandboxID
	return f.pending
}

func (f *fakeGrantCoordinator) Approve(_ context.Context, sandboxID, key string) (*model.CapabilityGrant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.approveSbx, f.approveKey = sandboxID, key
	if f.approveErr != nil {
		return nil, f.approveErr
	}
	return f.grant, nil
}

// TestDaemon_GrantsKind_NotServedWhenUnwired verifies the grants verb reports
// itself unserved when no coordinator is wired (e.g. off Linux). Refs: FR-17.12
func TestDaemon_GrantsKind_NotServedWhenUnwired(t *testing.T) {
	skipUnsupportedHostIPC(t)
	cfg, _ := dispatchConfig(t, &fakeDispatcher{}) // cfg.Grants left nil
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runDaemon(ctx, t, cfg)

	conn := dialAuthed(t, cfg.SocketPath)
	defer func() { _ = conn.Close() }()
	require.NoError(t, controlproto.WriteRequest(conn, &controlproto.Request{
		Kind: controlproto.KindGrants, Grants: &controlproto.TaskRef{TaskID: "MGIT-1"},
	}))
	resp, err := controlproto.ReadResponse(conn)
	require.NoError(t, err)
	assert.NotEmpty(t, resp.Error, "grants is not served without a coordinator")

	cancel()
	require.NoError(t, <-done)
}

// TestDaemon_Grants_ResolvesTaskAndLists verifies the grants verb resolves
// task->sandbox via the service, then returns the host-observed pending
// requests for that sandbox ID. Refs: FR-17.12, SEC-05
func TestDaemon_Grants_ResolvesTaskAndLists(t *testing.T) {
	skipUnsupportedHostIPC(t)
	gc := &fakeGrantCoordinator{pending: []model.CapabilityRequest{{
		Capability: "egress", ObservedDestIP: "203.0.113.7", ObservedDestPort: 443,
	}}}
	cfg, _ := dispatchConfig(t, &fakeDispatcher{})
	cfg.Grants = gc
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runDaemon(ctx, t, cfg)

	conn := dialAuthed(t, cfg.SocketPath)
	defer func() { _ = conn.Close() }()
	require.NoError(t, controlproto.WriteRequest(conn, &controlproto.Request{
		Kind: controlproto.KindGrants, Grants: &controlproto.TaskRef{TaskID: "MGIT-9"},
	}))
	resp, err := controlproto.ReadResponse(conn)
	require.NoError(t, err)
	require.Empty(t, resp.Error)
	require.Len(t, resp.Pending, 1)
	assert.Equal(t, "203.0.113.7", resp.Pending[0].DestIP)
	assert.Equal(t, 443, resp.Pending[0].DestPort)
	assert.Equal(t, "203.0.113.7:443", resp.Pending[0].Key)
	// The dispatch resolved the task to the host-owned sandbox ID, never guest text.
	assert.Equal(t, "01JXSBSANDBOX", gc.pendingFor)

	cancel()
	require.NoError(t, <-done)
}

// TestDaemon_Grant_ApprovesResolvedSandbox verifies the grant verb resolves the
// task and approves the keyed request against the host-owned sandbox ID,
// returning the granted destination. Refs: FR-17.12, SEC-05
func TestDaemon_Grant_ApprovesResolvedSandbox(t *testing.T) {
	skipUnsupportedHostIPC(t)
	gc := &fakeGrantCoordinator{grant: &model.CapabilityGrant{
		Capability: "egress", ObservedDestIP: "203.0.113.7", ObservedDestPort: 443,
	}}
	cfg, _ := dispatchConfig(t, &fakeDispatcher{})
	cfg.Grants = gc
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runDaemon(ctx, t, cfg)

	conn := dialAuthed(t, cfg.SocketPath)
	defer func() { _ = conn.Close() }()
	require.NoError(t, controlproto.WriteRequest(conn, &controlproto.Request{
		Kind:  controlproto.KindGrant,
		Grant: &controlproto.GrantArgs{TaskID: "MGIT-9", Key: "203.0.113.7:443"},
	}))
	resp, err := controlproto.ReadResponse(conn)
	require.NoError(t, err)
	require.Empty(t, resp.Error)
	require.NotNil(t, resp.Granted)
	assert.Equal(t, "203.0.113.7", resp.Granted.DestIP)
	assert.Equal(t, "01JXSBSANDBOX", gc.approveSbx)
	assert.Equal(t, "203.0.113.7:443", gc.approveKey)

	cancel()
	require.NoError(t, <-done)
}

// TestDaemon_Grant_ApproveError_ReturnedAsResponseError verifies a coordinator
// rejection (unknown key) surfaces as a control error, not a crash. Refs: FR-17.12
func TestDaemon_Grant_ApproveError_ReturnedAsResponseError(t *testing.T) {
	skipUnsupportedHostIPC(t)
	gc := &fakeGrantCoordinator{approveErr: model.ErrCapabilityGrantNotFound}
	cfg, _ := dispatchConfig(t, &fakeDispatcher{})
	cfg.Grants = gc
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runDaemon(ctx, t, cfg)

	conn := dialAuthed(t, cfg.SocketPath)
	defer func() { _ = conn.Close() }()
	require.NoError(t, controlproto.WriteRequest(conn, &controlproto.Request{
		Kind:  controlproto.KindGrant,
		Grant: &controlproto.GrantArgs{TaskID: "MGIT-9", Key: "203.0.113.9:443"},
	}))
	resp, err := controlproto.ReadResponse(conn)
	require.NoError(t, err)
	assert.NotEmpty(t, resp.Error, "an unknown key is rejected as a response error")

	cancel()
	require.NoError(t, <-done)
}
