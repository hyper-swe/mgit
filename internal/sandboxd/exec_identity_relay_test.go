package sandboxd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/controlproto"
	"github.com/hyper-swe/mgit/internal/model"
)

// The daemon relays the identity echo and its verdict to the client in the
// terminal result frame, beside the exit code, so the verb that ran the
// command can say what it ran as. Refs: MGIT-151
func TestDaemon_Exec_RelaysTheIdentityVerdict(t *testing.T) {
	skipUnsupportedHostIPC(t)
	agent := model.GuestIdentity{UID: 501, GID: 20, Name: "agent", Home: "/home/agent"}
	verdict := model.VerdictOnExecIdentity(&agent, nil)
	svc := &fakeDispatcher{execResult: &model.ExecResult{ExitCode: 0, Identity: &verdict}}
	cfg, _ := dispatchConfig(t, svc)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runDaemon(ctx, t, cfg)
	conn := dialAuthed(t, cfg.SocketPath)
	defer func() { _ = conn.Close() }()
	require.NoError(t, controlproto.WriteRequest(conn, &controlproto.Request{
		Kind: controlproto.KindExec,
		Exec: &controlproto.ExecArgs{TaskID: "MGIT-151", Exec: model.ExecRequest{Command: []string{"id"}}},
	}))
	_, _, result := readExec(t, conn)
	require.NotNil(t, result.Result.Identity, "the verdict rides the result frame")
	assert.False(t, result.Result.Identity.Verified)
	assert.Contains(t, result.Result.Identity.Reason, "did not report")
	require.NotNil(t, result.Result.Identity.Asked)
	assert.Equal(t, agent, *result.Result.Identity.Asked)
	cancel()
	require.NoError(t, <-done)
}
