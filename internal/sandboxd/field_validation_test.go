package sandboxd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/controlproto"
	"github.com/hyper-swe/mgit/internal/model"
)

// TestDaemon_FieldValidationFailure_NamesTheFieldAndTheRule: a request the
// model refuses on one field reaches its sender with the field and the rule
// (`task_id: invalid task id "T99": must match …`), because the sender can
// fix that; the bare "invalid request" is kept for frames that cannot be
// decoded at all, where naming parts would be inventing them
// (TestDaemon_Handshake_MalformedHello_FailsClosed pins that side).
// Refs: MGIT-207, FR-17.34
func TestDaemon_FieldValidationFailure_NamesTheFieldAndTheRule(t *testing.T) {
	skipUnsupportedHostIPC(t)
	cfg, _ := dispatchConfig(t, &fakeDispatcher{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runDaemon(ctx, t, cfg)

	conn := dialGreetedOnly(t, cfg.SocketPath)
	defer func() { _ = conn.Close() }()
	require.NotNil(t, sayHello(t, conn, controlproto.ProtocolVersion).Hello)
	require.NoError(t, controlproto.WriteRequest(conn, &controlproto.Request{
		Kind:   controlproto.KindLaunch,
		Launch: &model.SandboxLaunchOptions{TaskID: "T99", WorktreePath: "/w"},
	}))
	resp, err := controlproto.ReadResponse(conn)
	require.NoError(t, err)

	assert.NotEqual(t, "invalid request", resp.Error, "a field-level refusal is not an undecodable frame")
	assert.Contains(t, resp.Error, "task_id", "the field is named")
	assert.Contains(t, resp.Error, `"T99"`, "the offending value is quoted")
	assert.Contains(t, resp.Error, "must match", "the rule is stated")

	cancel()
	require.NoError(t, <-done)
}
