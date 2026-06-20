// Package sandboxd dispatch tests verify the daemon serves sandbox
// operations over the control plane after auth+greeting, and fails closed
// on hostile input without crashing or stranding VMs (MGIT-11.10.8
// acceptance + security audit). Refs: FR-17.34
package sandboxd

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/controlproto"
	"github.com/hyper-swe/mgit/internal/execwire"
	"github.com/hyper-swe/mgit/internal/model"
)

// fakeDispatcher records calls and returns canned results; panicOn makes
// the named operation panic (to exercise the daemon's recover()).
type fakeDispatcher struct {
	mu sync.Mutex

	registered  []model.SandboxLaunchOptions
	execTask    string
	execReq     model.ExecRequest
	removed     string
	removeForce bool
	statusTask  string

	execResult *model.ExecResult
	listResult []model.SandboxInfo
	opErr      error
	panicOn    string // "register" | "exec" | "list" | "remove" | "status"
}

func (f *fakeDispatcher) maybePanic(op string) {
	if f.panicOn == op {
		panic("induced handler panic in " + op)
	}
}

func (f *fakeDispatcher) Register(_ context.Context, opts model.SandboxLaunchOptions) (*model.SandboxInfo, error) {
	f.maybePanic("register")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.registered = append(f.registered, opts)
	if f.opErr != nil {
		return nil, f.opErr
	}
	return &model.SandboxInfo{
		ID: "01JXSBSANDBOX", TaskID: opts.TaskID, WorktreePath: opts.WorktreePath,
		Backend: model.BackendKVM, NetworkMode: opts.Network.Mode, State: model.StateCreated,
	}, nil
}

func (f *fakeDispatcher) Exec(_ context.Context, taskID string, req model.ExecRequest) (*model.ExecResult, error) {
	f.maybePanic("exec")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.execTask, f.execReq = taskID, req
	if f.opErr != nil {
		return nil, f.opErr
	}
	return f.execResult, nil
}

func (f *fakeDispatcher) List(_ context.Context) ([]model.SandboxInfo, error) {
	f.maybePanic("list")
	if f.opErr != nil {
		return nil, f.opErr
	}
	return f.listResult, nil
}

func (f *fakeDispatcher) Remove(_ context.Context, taskID string, force bool) error {
	f.maybePanic("remove")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed, f.removeForce = taskID, force
	return f.opErr
}

func (f *fakeDispatcher) Status(_ context.Context, taskID string) (*model.SandboxInfo, error) {
	f.maybePanic("status")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statusTask = taskID
	if f.opErr != nil {
		return nil, f.opErr
	}
	return &model.SandboxInfo{ID: "01JXSBSANDBOX", TaskID: taskID, State: model.StateRunning}, nil
}

func dispatchLaunchOpts() model.SandboxLaunchOptions {
	return model.SandboxLaunchOptions{
		TaskID: "MGIT-11.10.8", WorktreePath: "/work/a",
		ImageRef: "img@sha256:" + strings.Repeat("a", 64),
		Network:  model.NetworkPolicy{Mode: model.NetworkModeNone},
	}
}

// dispatchConfig wires a daemon with a dispatcher service.
func dispatchConfig(t *testing.T, svc SandboxDispatcher) (Config, *syncBuffer) {
	t.Helper()
	cfg, logs := testConfig(t, newFakeManager())
	cfg.IdleGrace = time.Hour
	cfg.Service = svc
	return cfg, logs
}

// dialAuthed dials the daemon, consumes the greeting, and returns the live
// connection ready to carry one control request.
func dialAuthed(t *testing.T, socketPath string) net.Conn {
	t.Helper()
	conn := waitForSocket(t, socketPath)
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(2*time.Second)))
	buf := make([]byte, len(greeting))
	_, err := io.ReadFull(conn, buf)
	require.NoError(t, err)
	require.Equal(t, greeting, string(buf), "greeting precedes any request")
	require.NoError(t, conn.SetDeadline(time.Now().Add(2*time.Second)))
	return conn
}

// readExec drains an exec frame stream into stdout, stderr, and the result.
func readExec(t *testing.T, conn net.Conn) (stdout, stderr []byte, result execwire.ResultFrame) {
	t.Helper()
	for {
		kind, payload, err := execwire.ReadFrame(conn)
		require.NoError(t, err)
		switch kind {
		case execwire.FrameStdout:
			stdout = append(stdout, payload...)
		case execwire.FrameStderr:
			stderr = append(stderr, payload...)
		case execwire.FrameResult:
			require.NoError(t, json.Unmarshal(payload, &result))
			return stdout, stderr, result
		default:
			t.Fatalf("unexpected exec frame kind %#x", kind)
		}
	}
}

// TestDaemon_Exec_RoutesToService verifies an exec request reaches the
// service and its output + exit code stream back as execwire frames.
func TestDaemon_Exec_RoutesToService(t *testing.T) {
	skipUnsupportedHostIPC(t)
	svc := &fakeDispatcher{execResult: &model.ExecResult{
		Stdout: []byte("hello\n"), Stderr: []byte("warn\n"), ExitCode: 7,
	}}
	cfg, _ := dispatchConfig(t, svc)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runDaemon(ctx, t, cfg)

	conn := dialAuthed(t, cfg.SocketPath)
	defer func() { _ = conn.Close() }()
	require.NoError(t, controlproto.WriteRequest(conn, &controlproto.Request{
		Kind: controlproto.KindExec,
		Exec: &controlproto.ExecArgs{TaskID: "MGIT-11.10.8", Exec: model.ExecRequest{Command: []string{"echo", "hi"}}},
	}))

	stdout, stderr, result := readExec(t, conn)
	assert.Equal(t, "hello\n", string(stdout))
	assert.Equal(t, "warn\n", string(stderr))
	assert.Equal(t, 7, result.Result.ExitCode)
	assert.Empty(t, result.Error)
	assert.Equal(t, "MGIT-11.10.8", svc.execTask, "exec routed to the service")
	assert.Equal(t, []string{"echo", "hi"}, svc.execReq.Command)

	cancel()
	require.NoError(t, <-done)
}

// TestDaemon_Exec_SetupFailure_ResultFrameCarriesError verifies an exec
// that fails to start comes back as a terminal result frame with an error
// string (clean end-of-stream), not a hang or crash.
func TestDaemon_Exec_SetupFailure_ResultFrameCarriesError(t *testing.T) {
	skipUnsupportedHostIPC(t)
	svc := &fakeDispatcher{opErr: model.ErrSandboxNotFound}
	cfg, _ := dispatchConfig(t, svc)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runDaemon(ctx, t, cfg)

	conn := dialAuthed(t, cfg.SocketPath)
	defer func() { _ = conn.Close() }()
	require.NoError(t, controlproto.WriteRequest(conn, &controlproto.Request{
		Kind: controlproto.KindExec,
		Exec: &controlproto.ExecArgs{TaskID: "MGIT-x", Exec: model.ExecRequest{Command: []string{"true"}}},
	}))
	_, _, result := readExec(t, conn)
	assert.NotEmpty(t, result.Error, "an exec setup failure is reported in the result frame")

	cancel()
	require.NoError(t, <-done)
}

// TestDaemon_Exec_LargeOutput_ChunkedRelay verifies output larger than one
// relay chunk is reassembled intact across multiple frames.
func TestDaemon_Exec_LargeOutput_ChunkedRelay(t *testing.T) {
	skipUnsupportedHostIPC(t)
	big := make([]byte, execRelayChunkBytes*2+123)
	for i := range big {
		big[i] = byte('a' + i%26)
	}
	svc := &fakeDispatcher{execResult: &model.ExecResult{Stdout: big}}
	cfg, _ := dispatchConfig(t, svc)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runDaemon(ctx, t, cfg)

	conn := dialAuthed(t, cfg.SocketPath)
	defer func() { _ = conn.Close() }()
	require.NoError(t, controlproto.WriteRequest(conn, &controlproto.Request{
		Kind: controlproto.KindExec,
		Exec: &controlproto.ExecArgs{TaskID: "MGIT-1", Exec: model.ExecRequest{Command: []string{"cat"}}},
	}))
	stdout, _, result := readExec(t, conn)
	assert.Equal(t, big, stdout, "chunked output reassembles intact")
	assert.Equal(t, 0, result.Result.ExitCode)

	cancel()
	require.NoError(t, <-done)
}

// TestDaemon_LandKind_NotServedHere verifies the land kind (served by the
// land orchestrator, MGIT-11.10.10) is rejected with an error, not a crash.
func TestDaemon_LandKind_NotServedHere(t *testing.T) {
	skipUnsupportedHostIPC(t)
	cfg, _ := dispatchConfig(t, &fakeDispatcher{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runDaemon(ctx, t, cfg)

	conn := dialAuthed(t, cfg.SocketPath)
	defer func() { _ = conn.Close() }()
	require.NoError(t, controlproto.WriteRequest(conn, &controlproto.Request{
		Kind: controlproto.KindLand, Land: &controlproto.TaskRef{TaskID: "MGIT-1"},
	}))
	resp, err := controlproto.ReadResponse(conn)
	require.NoError(t, err)
	assert.NotEmpty(t, resp.Error, "land is not served by this dispatcher")

	cancel()
	require.NoError(t, <-done)
}

// TestDaemon_Launch_RegistersAndAudits verifies a launch request reaches
// the service's Register (which records the created audit) and the created
// sandbox info is returned.
func TestDaemon_Launch_RegistersAndAudits(t *testing.T) {
	skipUnsupportedHostIPC(t)
	svc := &fakeDispatcher{}
	cfg, _ := dispatchConfig(t, svc)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runDaemon(ctx, t, cfg)

	conn := dialAuthed(t, cfg.SocketPath)
	defer func() { _ = conn.Close() }()
	require.NoError(t, controlproto.WriteRequest(conn, &controlproto.Request{
		Kind: controlproto.KindLaunch, Launch: ptrOpts(dispatchLaunchOpts()),
	}))

	resp, err := controlproto.ReadResponse(conn)
	require.NoError(t, err)
	require.Empty(t, resp.Error)
	require.NotNil(t, resp.Sandbox)
	assert.Equal(t, "MGIT-11.10.8", resp.Sandbox.TaskID)
	require.Len(t, svc.registered, 1, "the launch was registered via the service")

	cancel()
	require.NoError(t, <-done)
}

// TestDaemon_List_Remove_Status verifies the three management ops dispatch
// to the service and return their typed results.
func TestDaemon_List_Remove_Status(t *testing.T) {
	skipUnsupportedHostIPC(t)
	svc := &fakeDispatcher{listResult: []model.SandboxInfo{
		{ID: "01JXSBA", TaskID: "MGIT-1", State: model.StateRunning},
	}}
	cfg, _ := dispatchConfig(t, svc)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runDaemon(ctx, t, cfg)

	t.Run("list", func(t *testing.T) {
		conn := dialAuthed(t, cfg.SocketPath)
		defer func() { _ = conn.Close() }()
		require.NoError(t, controlproto.WriteRequest(conn, &controlproto.Request{Kind: controlproto.KindList}))
		resp, err := controlproto.ReadResponse(conn)
		require.NoError(t, err)
		require.Len(t, resp.List, 1)
		assert.Equal(t, "MGIT-1", resp.List[0].TaskID)
	})

	t.Run("status", func(t *testing.T) {
		conn := dialAuthed(t, cfg.SocketPath)
		defer func() { _ = conn.Close() }()
		require.NoError(t, controlproto.WriteRequest(conn, &controlproto.Request{
			Kind: controlproto.KindStatus, Status: &controlproto.TaskRef{TaskID: "MGIT-9"},
		}))
		resp, err := controlproto.ReadResponse(conn)
		require.NoError(t, err)
		require.NotNil(t, resp.Sandbox)
		assert.Equal(t, "MGIT-9", resp.Sandbox.TaskID)
		assert.Equal(t, "MGIT-9", svc.statusTask)
	})

	t.Run("remove", func(t *testing.T) {
		conn := dialAuthed(t, cfg.SocketPath)
		defer func() { _ = conn.Close() }()
		require.NoError(t, controlproto.WriteRequest(conn, &controlproto.Request{
			Kind: controlproto.KindRemove, Remove: &controlproto.RemoveArgs{TaskID: "MGIT-3", Force: true},
		}))
		resp, err := controlproto.ReadResponse(conn)
		require.NoError(t, err)
		assert.Empty(t, resp.Error)
		assert.Equal(t, "MGIT-3", svc.removed)
		assert.True(t, svc.removeForce)
	})

	cancel()
	require.NoError(t, <-done)
}

// TestDaemon_OpError_ReturnedAsResponseError verifies a service failure
// comes back as a control response error, not a crash.
func TestDaemon_OpError_ReturnedAsResponseError(t *testing.T) {
	skipUnsupportedHostIPC(t)
	svc := &fakeDispatcher{opErr: model.ErrSandboxNotFound}
	cfg, _ := dispatchConfig(t, svc)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runDaemon(ctx, t, cfg)

	conn := dialAuthed(t, cfg.SocketPath)
	defer func() { _ = conn.Close() }()
	require.NoError(t, controlproto.WriteRequest(conn, &controlproto.Request{
		Kind: controlproto.KindStatus, Status: &controlproto.TaskRef{TaskID: "MGIT-x"},
	}))
	resp, err := controlproto.ReadResponse(conn)
	require.NoError(t, err)
	assert.NotEmpty(t, resp.Error, "a service error is surfaced as a response error")

	cancel()
	require.NoError(t, <-done)
}

// TestDaemon_MalformedRequest_FailsClosedNoCrash verifies hostile/garbage
// and oversized framing is rejected without crashing the daemon, which
// keeps serving subsequent well-formed clients.
func TestDaemon_MalformedRequest_FailsClosedNoCrash(t *testing.T) {
	skipUnsupportedHostIPC(t)
	svc := &fakeDispatcher{listResult: nil}
	cfg, logs := dispatchConfig(t, svc)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runDaemon(ctx, t, cfg)

	// Garbage frame: unknown kind + a bogus length.
	conn := dialAuthed(t, cfg.SocketPath)
	_, err := conn.Write([]byte{0xff, 0x7f, 0xff, 0xff, 0xff})
	require.NoError(t, err)
	_ = conn.Close()

	require.Eventually(t, func() bool {
		return strings.Contains(logs.String(), `"request_rejected"`)
	}, 2*time.Second, 10*time.Millisecond, "the malformed request must be audited")

	// The daemon is unharmed: a well-formed client still gets served.
	conn2 := dialAuthed(t, cfg.SocketPath)
	defer func() { _ = conn2.Close() }()
	require.NoError(t, controlproto.WriteRequest(conn2, &controlproto.Request{Kind: controlproto.KindList}))
	resp, err := controlproto.ReadResponse(conn2)
	require.NoError(t, err, "the daemon survived the malformed request")
	assert.Empty(t, resp.Error)

	cancel()
	require.NoError(t, <-done)
}

// TestDaemon_PanicInHandler_DaemonSurvives verifies a handler panic is
// recovered: the daemon keeps serving and never crashes (a crash would
// skip drain and strand running VMs). Refs: MGIT-11.10.8 (security audit)
func TestDaemon_PanicInHandler_DaemonSurvives(t *testing.T) {
	skipUnsupportedHostIPC(t)
	svc := &fakeDispatcher{panicOn: "list"}
	cfg, logs := dispatchConfig(t, svc)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runDaemon(ctx, t, cfg)

	// This request panics inside the handler.
	conn := dialAuthed(t, cfg.SocketPath)
	require.NoError(t, controlproto.WriteRequest(conn, &controlproto.Request{Kind: controlproto.KindList}))
	_ = conn.Close()

	require.Eventually(t, func() bool {
		return strings.Contains(logs.String(), `"handler_panic"`)
	}, 2*time.Second, 10*time.Millisecond, "the panic must be recovered + audited")

	// The daemon is still alive and authenticating.
	conn2 := dialAuthed(t, cfg.SocketPath)
	defer func() { _ = conn2.Close() }()

	cancel()
	require.NoError(t, <-done, "the daemon drained cleanly; it never crashed")
}

// TestDaemon_GreetOnly_NoServiceServesNothing verifies a build without a
// wired service still authenticates and greets but serves no operations
// (the connection closes after the greeting).
func TestDaemon_GreetOnly_NoServiceServesNothing(t *testing.T) {
	skipUnsupportedHostIPC(t)
	cfg, _ := testConfig(t, newFakeManager())
	cfg.IdleGrace = time.Hour // Service stays nil
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runDaemon(ctx, t, cfg)

	conn := dialAuthed(t, cfg.SocketPath)
	defer func() { _ = conn.Close() }()
	// No request is served: a read returns EOF once the handler returns.
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(2*time.Second)))
	buf := make([]byte, 1)
	_, err := conn.Read(buf)
	assert.ErrorIs(t, err, io.EOF, "a greet-only daemon closes after greeting")

	cancel()
	require.NoError(t, <-done)
}

func ptrOpts(o model.SandboxLaunchOptions) *model.SandboxLaunchOptions { return &o }

// failConn is a net.Conn whose writes always fail, to exercise the relay's
// broken-connection branches deterministically.
type failConn struct{ net.Conn }

func (failConn) Write([]byte) (int, error)        { return 0, assert.AnError }
func (failConn) SetWriteDeadline(time.Time) error { return nil }

// TestDispatch_WriteErrors_LoggedNoCrash verifies the exec relay logs and
// returns cleanly when the connection write fails mid-stream (the client
// vanished) rather than panicking.
func TestDispatch_WriteErrors_LoggedNoCrash(t *testing.T) {
	cfg, logs := dispatchConfig(t, &fakeDispatcher{})
	d, err := New(cfg)
	require.NoError(t, err)

	assert.False(t, d.relayChunks(failConn{}, execwire.FrameStdout, []byte("data")),
		"a failed frame write reports failure")
	d.writeResultFrame(failConn{}, execwire.Result{ExitCode: 0}, "")
	d.writeResponse(failConn{}, &controlproto.Response{})
	assert.Contains(t, logs.String(), `"write_error"`, "broken-connection writes are audited")
}

// TestDaemon_ConcurrencyCap_RejectsBeyondLimit verifies that beyond
// MaxConns the daemon rejects fast (no greeting) instead of spawning
// unbounded goroutines. Refs: MGIT-11.10.8 (security audit)
func TestDaemon_ConcurrencyCap_RejectsBeyondLimit(t *testing.T) {
	skipUnsupportedHostIPC(t)
	cfg, logs := dispatchConfig(t, &fakeDispatcher{})
	cfg.MaxConns = 1
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runDaemon(ctx, t, cfg)

	// Hold the single slot: this client authenticates and then leaves the
	// handler blocked in ReadRequest (no request sent).
	hold := dialAuthed(t, cfg.SocketPath)
	defer func() { _ = hold.Close() }()

	// A second connection arrives at capacity and is rejected fast.
	require.Eventually(t, func() bool {
		c, err := net.Dial("unix", cfg.SocketPath)
		if err != nil {
			return false
		}
		defer func() { _ = c.Close() }()
		return strings.Contains(logs.String(), `"conn_rejected"`)
	}, 2*time.Second, 20*time.Millisecond, "the over-capacity connection must be rejected")

	cancel()
	require.NoError(t, <-done)
}
