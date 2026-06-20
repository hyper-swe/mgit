package sandboxd

import (
	"bytes"
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// newClientForDaemon starts a daemon with the given dispatcher and returns
// a client wired to its socket.
func newClientForDaemon(t *testing.T, svc SandboxDispatcher) (*Client, func()) {
	t.Helper()
	skipUnsupportedHostIPC(t)
	cfg, _ := dispatchConfig(t, svc)
	ctx, cancel := context.WithCancel(context.Background())
	done := runDaemon(ctx, t, cfg)
	_ = waitForSocket(t, cfg.SocketPath).Close()
	client := NewClient(cfg.SocketPath, time.Now)
	return client, func() { cancel(); <-done }
}

// TestClient_LaunchListStatusRemove_RoundTrip verifies each non-streaming
// verb completes a full request/response against a live daemon.
func TestClient_LaunchListStatusRemove_RoundTrip(t *testing.T) {
	svc := &fakeDispatcher{listResult: []model.SandboxInfo{{ID: "01J", TaskID: "MGIT-1", State: model.StateRunning}}}
	client, stop := newClientForDaemon(t, svc)
	defer stop()
	ctx := context.Background()

	info, err := client.Launch(ctx, dispatchLaunchOpts())
	require.NoError(t, err)
	require.NotNil(t, info)
	assert.Equal(t, "MGIT-11.10.8", info.TaskID)

	list, err := client.List(ctx)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "MGIT-1", list[0].TaskID)

	st, err := client.Status(ctx, "MGIT-9")
	require.NoError(t, err)
	assert.Equal(t, "MGIT-9", st.TaskID)

	require.NoError(t, client.Remove(ctx, "MGIT-3", true))
	assert.Equal(t, "MGIT-3", svc.removed)
	assert.True(t, svc.removeForce)
}

// TestClient_Exec_StreamsAndPropagatesExit verifies the client copies the
// exec output frames to its writers and returns the guest exit code.
func TestClient_Exec_StreamsAndPropagatesExit(t *testing.T) {
	svc := &fakeDispatcher{execResult: &model.ExecResult{
		Stdout: []byte("out\n"), Stderr: []byte("err\n"), ExitCode: 42,
	}}
	client, stop := newClientForDaemon(t, svc)
	defer stop()

	var stdout, stderr bytes.Buffer
	code, err := client.Exec(context.Background(), "MGIT-1",
		model.ExecRequest{Command: []string{"sh", "-c", "echo out; echo err 1>&2; exit 42"}}, &stdout, &stderr)
	require.NoError(t, err)
	assert.Equal(t, 42, code, "the guest exit code propagates")
	assert.Equal(t, "out\n", stdout.String())
	assert.Equal(t, "err\n", stderr.String())
}

// TestClient_Exec_SetupFailure_ReturnsError verifies a supervisor-level
// exec failure surfaces as an error with a -1 exit code.
func TestClient_Exec_SetupFailure_ReturnsError(t *testing.T) {
	client, stop := newClientForDaemon(t, &fakeDispatcher{opErr: model.ErrSandboxNotFound})
	defer stop()

	code, err := client.Exec(context.Background(), "MGIT-x",
		model.ExecRequest{Command: []string{"true"}}, &bytes.Buffer{}, &bytes.Buffer{})
	require.Error(t, err)
	assert.Equal(t, -1, code)
}

// errWriter fails every write, standing in for a broken output sink.
type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, assert.AnError }

// TestClient_Exec_OutputWriteError_Surfaces verifies a failing stdout sink
// surfaces as an error rather than being silently dropped.
func TestClient_Exec_OutputWriteError_Surfaces(t *testing.T) {
	svc := &fakeDispatcher{execResult: &model.ExecResult{Stdout: []byte("data"), ExitCode: 0}}
	client, stop := newClientForDaemon(t, svc)
	defer stop()

	code, err := client.Exec(context.Background(), "MGIT-1",
		model.ExecRequest{Command: []string{"true"}}, errWriter{}, &bytes.Buffer{})
	require.Error(t, err)
	assert.Equal(t, -1, code)
}

// TestClient_OpError_Surfaces verifies a service error comes back as a
// client error (not a silent success).
func TestClient_OpError_Surfaces(t *testing.T) {
	client, stop := newClientForDaemon(t, &fakeDispatcher{opErr: model.ErrSandboxNotFound})
	defer stop()
	_, err := client.Status(context.Background(), "MGIT-x")
	require.Error(t, err)
}

// TestClient_NoDaemon_FailsClosed verifies dialing a dead socket fails
// clearly (no fallback): every verb errors, nothing runs.
func TestClient_NoDaemon_FailsClosed(t *testing.T) {
	client := NewClient(shortSocketPath(t), time.Now) // never served
	ctx := context.Background()

	_, err := client.List(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "dial daemon")

	code, execErr := client.Exec(ctx, "MGIT-1", model.ExecRequest{Command: []string{"true"}}, &bytes.Buffer{}, &bytes.Buffer{})
	require.Error(t, execErr)
	assert.Equal(t, -1, code, "no exit code is invented when the daemon is unreachable")
}

// TestClient_Squatter_NoGreeting_Rejected verifies a socket that accepts
// but never greets is rejected as a squatter.
func TestClient_Squatter_NoGreeting_Rejected(t *testing.T) {
	skipUnsupportedHostIPC(t)
	path := shortSocketPath(t)
	ln, err := net.Listen("unix", path)
	require.NoError(t, err)
	defer func() { _ = ln.Close() }()
	// A squatter that accepts and stays silent.
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			time.Sleep(50 * time.Millisecond)
			_ = conn.Close()
		}
	}()

	client := NewClient(path, time.Now)
	_, err = client.List(context.Background())
	require.Error(t, err)
	assert.Contains(t, strings.ToLower(err.Error()), "greet")
}
