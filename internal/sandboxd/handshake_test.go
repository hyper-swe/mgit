// Package sandboxd handshake tests cover the CLI<->daemon version exchange:
// what a current pair does, what each side does when it meets a build that
// predates the handshake, and — the reason the ticket is release-gating —
// that a version mismatch can never be rendered as a guest failure.
// Refs: MGIT-136, MGIT-133, MGIT-118
package sandboxd

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/controlproto"
	"github.com/hyper-swe/mgit/internal/execwire"
	"github.com/hyper-swe/mgit/internal/model"
)

// dialGreetedOnly dials the daemon and consumes ONLY the greeting — the exact
// state an mgit 0.5.x client is in when it writes its first request, because
// that build has no handshake to perform. Every "old client" test starts here.
// Refs: MGIT-136
func dialGreetedOnly(t *testing.T, socketPath string) net.Conn {
	t.Helper()
	conn := waitForSocket(t, socketPath)
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(2*time.Second)))
	buf := make([]byte, len(greeting))
	_, err := io.ReadFull(conn, buf)
	require.NoError(t, err)
	require.Equal(t, greeting, string(buf),
		"the greeting is byte-identical, so a 0.5.x client still gets this far")
	require.NoError(t, conn.SetDeadline(time.Now().Add(2*time.Second)))
	return conn
}

// sayHello performs the client half of the handshake at a chosen protocol
// number, so a test can drive the boundary of the compatibility rule without
// rebuilding a binary. Refs: MGIT-136
func sayHello(t *testing.T, conn net.Conn, protocol int) *controlproto.Response {
	t.Helper()
	require.NoError(t, controlproto.WriteRequest(conn, &controlproto.Request{
		Kind:  controlproto.KindHello,
		Hello: &controlproto.HelloArgs{Protocol: protocol, Version: "test-client"},
	}))
	resp, err := controlproto.ReadResponse(conn)
	require.NoError(t, err)
	return resp
}

// TestDaemon_Handshake_MatchingProtocol_ServesTheVerb verifies the happy pair:
// a client that states this build's protocol gets the daemon's own version
// back and its verb served on the same connection. Refs: MGIT-136
func TestDaemon_Handshake_MatchingProtocol_ServesTheVerb(t *testing.T) {
	skipUnsupportedHostIPC(t)
	svc := &fakeDispatcher{listResult: []model.SandboxInfo{{ID: "01JXSB", TaskID: "MGIT-136"}}}
	cfg, _ := dispatchConfig(t, svc)
	cfg.Version = "0.6.0 (commit: deadbee)"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runDaemon(ctx, t, cfg)

	conn := dialGreetedOnly(t, cfg.SocketPath)
	defer func() { _ = conn.Close() }()

	hello := sayHello(t, conn, controlproto.ProtocolVersion)
	require.NotNil(t, hello.Hello, "the daemon states its own version")
	assert.Equal(t, controlproto.ProtocolVersion, hello.Hello.Protocol)
	assert.Equal(t, "0.6.0 (commit: deadbee)", hello.Hello.Version)
	assert.Empty(t, hello.Error, "a matching pair is not an error")

	require.NoError(t, controlproto.WriteRequest(conn, &controlproto.Request{Kind: controlproto.KindList}))
	resp, err := controlproto.ReadResponse(conn)
	require.NoError(t, err)
	require.Len(t, resp.List, 1, "the verb is served on the same connection")

	cancel()
	require.NoError(t, <-done)
}

// TestDaemon_Handshake_MismatchedProtocol_RefusesAndNamesBothSides drives the
// compatibility rule at BOTH boundaries — one below and one above — and
// asserts the refusal names each side's version and never serves the verb.
// Refs: MGIT-136
func TestDaemon_Handshake_MismatchedProtocol_RefusesAndNamesBothSides(t *testing.T) {
	skipUnsupportedHostIPC(t)
	tests := []struct {
		name  string
		proto int
	}{
		{"one_below", controlproto.ProtocolVersion - 1},
		{"one_above", controlproto.ProtocolVersion + 1},
		{"legacy_number", controlproto.LegacyProtocol},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := &fakeDispatcher{}
			cfg, _ := dispatchConfig(t, svc)
			cfg.Version = "0.6.0 (commit: deadbee)"
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := runDaemon(ctx, t, cfg)

			conn := dialGreetedOnly(t, cfg.SocketPath)
			defer func() { _ = conn.Close() }()
			hello := sayHello(t, conn, tt.proto)

			require.NotNil(t, hello.Hello, "the daemon still states its own version")
			assert.Contains(t, hello.Error, model.ErrSandboxVersionSkew.Error())
			assert.Contains(t, hello.Error, "upgrade both")
			assert.Contains(t, hello.Error, "0.6.0 (commit: deadbee)", "the daemon's build is named")
			assert.Contains(t, hello.Error, "test-client", "the client's build is named")
			assert.NotContains(t, strings.ToLower(hello.Error), "memory",
				"a version mismatch says nothing about the guest's memory (MGIT-118)")

			// The verb must not be served. The daemon has already closed, so
			// the send may itself fail — either way nothing comes back and the
			// service is never reached.
			if werr := controlproto.WriteRequest(conn, &controlproto.Request{Kind: controlproto.KindList}); werr == nil {
				_, rerr := controlproto.ReadResponse(conn)
				assert.Error(t, rerr, "a refused pair transacts nothing")
			}
			assert.Empty(t, svc.statusTask)

			cancel()
			require.NoError(t, <-done)
		})
	}
}

// TestDaemon_PreHandshakeClient_NonExecVerb_RefusalIsDecodable simulates the
// shape an mgit 0.5.x client produces — greeting consumed, then a verb frame
// with no hello — and verifies the daemon answers with a control RESPONSE,
// which is exactly what that client is waiting for and will print.
//
// This is why the refusal is shaped per verb rather than emitted eagerly after
// the greeting: an old client's next read depends on the verb it just sent.
// Refs: MGIT-136
func TestDaemon_PreHandshakeClient_NonExecVerb_RefusalIsDecodable(t *testing.T) {
	skipUnsupportedHostIPC(t)
	for _, kind := range []byte{controlproto.KindList, controlproto.KindStatus} {
		t.Run(string(kind), func(t *testing.T) {
			svc := &fakeDispatcher{}
			cfg, logs := dispatchConfig(t, svc)
			cfg.Version = "0.6.0 (commit: deadbee)"
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := runDaemon(ctx, t, cfg)

			conn := dialGreetedOnly(t, cfg.SocketPath)
			defer func() { _ = conn.Close() }()
			req := &controlproto.Request{Kind: kind}
			if kind == controlproto.KindStatus {
				req.Status = &controlproto.TaskRef{TaskID: "MGIT-136"}
			}
			require.NoError(t, controlproto.WriteRequest(conn, req))

			resp, err := controlproto.ReadResponse(conn)
			require.NoError(t, err, "the refusal is in the shape the old client decodes")
			assert.Contains(t, resp.Error, model.ErrSandboxVersionSkew.Error())
			assert.Contains(t, resp.Error, "control protocol 1", "the peer is named as pre-handshake")
			assert.Contains(t, resp.Error, "0.6.0 (commit: deadbee)")
			// The stale half of THIS pair is the client, and this daemon is
			// the new build serving the refusal. So the remedy is to upgrade
			// the CLI, and the reader must not be sent to kill a daemon that
			// is already current (MGIT-138).
			assert.Contains(t, resp.Error, "The stale half here is the mgit CLI")
			assert.NotContains(t, resp.Error, "pkill",
				"a daemon refusing an OLD CLI told the reader to kill itself")
			assert.Empty(t, svc.statusTask, "no verb is served to an unversioned peer")
			assert.Contains(t, logs.String(), "version_skew", "the refusal is audited")

			cancel()
			require.NoError(t, <-done)
		})
	}
}

// TestDaemon_PreHandshakeClient_Exec_RefusalIsFrameShaped is the one that
// matters most. An mgit 0.5.x `sandbox exec` reads execwire FRAMES after its
// request, so a response-shaped refusal would be misread as a frame — which is
// precisely how MGIT-136 was found: an unknown frame byte lands in that
// build's "guest lost mid-command" phase and prints a memory-cap advisory.
//
// The daemon therefore answers exec in frames: the full message on stderr,
// then a terminal result. Refs: MGIT-136, MGIT-118
func TestDaemon_PreHandshakeClient_Exec_RefusalIsFrameShaped(t *testing.T) {
	skipUnsupportedHostIPC(t)
	svc := &fakeDispatcher{}
	cfg, _ := dispatchConfig(t, svc)
	cfg.Version = "0.6.0 (commit: deadbee)"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runDaemon(ctx, t, cfg)

	conn := dialGreetedOnly(t, cfg.SocketPath)
	defer func() { _ = conn.Close() }()
	require.NoError(t, controlproto.WriteRequest(conn, &controlproto.Request{
		Kind: controlproto.KindExec,
		Exec: &controlproto.ExecArgs{TaskID: "MGIT-136", Exec: model.ExecRequest{Command: []string{"true"}}},
	}))

	_, stderr, result := readExec(t, conn)
	assert.Contains(t, string(stderr), model.ErrSandboxVersionSkew.Error(),
		"the old client prints this straight to its stderr")
	assert.Contains(t, string(stderr), "upgrade both")
	assert.Contains(t, result.Error, model.ErrSandboxVersionSkew.Error(),
		"the terminal frame names the skew, so the failure is not anonymous")
	assert.Equal(t, -1, result.Result.ExitCode)
	assert.Empty(t, svc.execTask, "nothing reached the guest")
	assert.NotContains(t, strings.ToLower(string(stderr)+result.Error), "memory")

	cancel()
	require.NoError(t, <-done)
}

// TestDaemon_Handshake_MalformedHello_FailsClosed verifies a hello that does
// not validate is rejected as a bad request rather than being read as a
// version. A malformed frame is not evidence of a version. Refs: MGIT-136
func TestDaemon_Handshake_MalformedHello_FailsClosed(t *testing.T) {
	skipUnsupportedHostIPC(t)
	cfg, _ := dispatchConfig(t, &fakeDispatcher{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runDaemon(ctx, t, cfg)

	conn := dialGreetedOnly(t, cfg.SocketPath)
	defer func() { _ = conn.Close() }()
	// Protocol 0 is not a version any build ever spoke.
	require.NoError(t, controlproto.WriteRequest(conn, &controlproto.Request{
		Kind: controlproto.KindHello, Hello: &controlproto.HelloArgs{Protocol: 0},
	}))
	resp, err := controlproto.ReadResponse(conn)
	require.NoError(t, err)
	assert.Equal(t, "invalid request", resp.Error)
	assert.Nil(t, resp.Hello)

	cancel()
	require.NoError(t, <-done)
}

// TestDaemon_Handshake_SecondHello_IsNotAVerb verifies the handshake is not a
// way to loop: after a successful exchange the connection carries exactly one
// verb, and a repeat hello is refused. Refs: MGIT-136, FR-17.34
func TestDaemon_Handshake_SecondHello_IsNotAVerb(t *testing.T) {
	skipUnsupportedHostIPC(t)
	cfg, _ := dispatchConfig(t, &fakeDispatcher{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runDaemon(ctx, t, cfg)

	conn := dialGreetedOnly(t, cfg.SocketPath)
	defer func() { _ = conn.Close() }()
	require.NotNil(t, sayHello(t, conn, controlproto.ProtocolVersion).Hello)
	second := sayHello(t, conn, controlproto.ProtocolVersion)
	assert.Equal(t, "invalid request", second.Error)
	assert.Nil(t, second.Hello)

	cancel()
	require.NoError(t, <-done)
}

// TestClient_Handshake_LegacyDaemon_ReportsOldDaemon_NotAStall verifies the
// direction MGIT-133 kept working: a CURRENT client meeting a daemon that
// predates the handshake. That daemon answers the hello with its generic
// "invalid request", and the client must read that as an OLD DAEMON — never as
// a wedged one, and never as anything about a guest. Refs: MGIT-136, MGIT-133
func TestClient_Handshake_LegacyDaemon_ReportsOldDaemon_NotAStall(t *testing.T) {
	skipUnsupportedHostIPC(t)
	socket := legacyDaemonSocket(t, func(conn net.Conn) {
		// Exactly what mgit 0.5.x does with an unknown request kind.
		_, _ = controlproto.ReadRequest(conn)
		_ = controlproto.WriteResponse(conn, &controlproto.Response{Error: "invalid request"})
	})

	cl := NewClient(socket, time.Now)
	_, err := cl.List(context.Background())
	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrSandboxVersionSkew)
	assert.Contains(t, err.Error(), "control protocol 1", "the daemon is named as pre-handshake")
	assert.Contains(t, err.Error(), "upgrade both")
	// Here the DAEMON is the stale half, so stopping it is exactly the fix
	// and the remedy names it (MGIT-138).
	assert.Contains(t, err.Error(), "pkill -f mgit-sandboxd")
	assert.NotContains(t, err.Error(), "did not greet",
		"a greeted daemon that is merely old is not reported as a squatter")
	assert.NotErrorIs(t, err, model.ErrSandboxDaemonUnresponsive,
		"an old daemon is not a stalled one")
}

// TestClient_Handshake_TransportFailure_IsNotReadAsSkew is the trap this
// ticket names explicitly: silence or a broken read must be reported as what
// it is. A peer that greets and then closes has told us nothing about its
// version, and guessing "skew" there would make every transport fault look
// like an upgrade problem. Refs: MGIT-136
func TestClient_Handshake_TransportFailure_IsNotReadAsSkew(t *testing.T) {
	skipUnsupportedHostIPC(t)
	socket := legacyDaemonSocket(t, func(conn net.Conn) { _ = conn.Close() })

	cl := NewClient(socket, time.Now)
	_, err := cl.List(context.Background())
	require.Error(t, err)
	assert.NotErrorIs(t, err, model.ErrSandboxVersionSkew,
		"a dropped connection is not evidence of a version")
	assert.Contains(t, err.Error(), "version handshake")
}

// TestClient_Handshake_FutureDaemon_NamesBothSides verifies the client refuses
// a daemon NEWER than itself just as firmly as an older one — the exact-match
// rule is symmetric, and the stale side here is the CLI. Refs: MGIT-136
func TestClient_Handshake_FutureDaemon_NamesBothSides(t *testing.T) {
	skipUnsupportedHostIPC(t)
	socket := legacyDaemonSocket(t, func(conn net.Conn) {
		_, _ = controlproto.ReadRequest(conn)
		_ = controlproto.WriteResponse(conn, &controlproto.Response{
			Hello: &controlproto.HelloResult{
				Protocol: controlproto.ProtocolVersion + 1, Version: "9.9.9 (commit: future)"},
		})
	})

	cl := NewClient(socket, time.Now)
	_, err := cl.List(context.Background())
	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrSandboxVersionSkew)
	assert.Contains(t, err.Error(), "9.9.9 (commit: future)")
	assert.Contains(t, err.Error(), "brew upgrade hyper-swe/tap/mgit")
	// The stale half is this CLI, so the remedy says so and does NOT send the
	// reader after a daemon that is newer than it is (MGIT-138).
	assert.Contains(t, err.Error(), "The stale half here is the mgit CLI")
	assert.NotContains(t, err.Error(), "pkill")
}

// TestClient_Handshake_Exec_SkewNeverReachesTheFrameLoop verifies an exec
// against an incompatible daemon fails at the handshake, before a single exec
// frame is read — so the skew can never be mistaken for a guest that stopped
// answering. Refs: MGIT-136, MGIT-118
func TestClient_Handshake_Exec_SkewNeverReachesTheFrameLoop(t *testing.T) {
	skipUnsupportedHostIPC(t)
	socket := legacyDaemonSocket(t, func(conn net.Conn) {
		_, _ = controlproto.ReadRequest(conn)
		_ = controlproto.WriteResponse(conn, &controlproto.Response{Error: "invalid request"})
		// Then behave like a beating daemon, to prove the client never got here.
		_ = execwire.WriteHeartbeat(conn)
	})

	cl := NewClient(socket, time.Now)
	var out, errOut strings.Builder
	code, err := cl.Exec(context.Background(), "MGIT-136",
		model.ExecRequest{Command: []string{"true"}}, &out, &errOut)
	require.Error(t, err)
	assert.Equal(t, -1, code)
	assert.ErrorIs(t, err, model.ErrSandboxVersionSkew)
	assert.NotContains(t, err.Error(), "unexpected exec frame",
		"the skew is settled before any frame is read")
	assert.Empty(t, errOut.String())
}

// TestClient_Handshake_PreMGIT133Daemon_RefusedBeforeTheFrameLoop is the pin
// that makes MGIT-138's removal safe, and it is the whole justification for it.
//
// The client no longer carries a "this daemon sends no beats, so drop the
// deadline and wait unbounded" fallback. The reason that is safe is the
// handshake: a daemon old enough to emit no beat is older than MGIT-133, and
// therefore older than the handshake, so it never reaches relayFrames. This
// peer proves it the hard way — it refuses the hello exactly as mgit 0.5.x does
// and then serves a PERFECTLY GOOD beat-less exec (output, then a zero exit) on
// the same connection. The removed fallback existed to consume precisely that
// stream. If the handshake ever stopped covering this peer, those frames would
// be read and the exec would succeed here instead of being refused.
// Refs: MGIT-138, MGIT-136, MGIT-133
func TestClient_Handshake_PreMGIT133Daemon_RefusedBeforeTheFrameLoop(t *testing.T) {
	skipUnsupportedHostIPC(t)
	socket := legacyDaemonSocket(t, func(conn net.Conn) {
		// mgit 0.5.x's answer to a request kind it does not know.
		_, _ = controlproto.ReadRequest(conn)
		_ = controlproto.WriteResponse(conn, &controlproto.Response{Error: "invalid request"})
		// Then serve the exec the way a pre-MGIT-133 daemon would: no beat,
		// ever, and a clean successful result.
		_ = execwire.WriteFrame(conn, execwire.FrameStdout, []byte("built\n"))
		payload, err := json.Marshal(execwire.ResultFrame{Result: execwire.Result{ExitCode: 0}})
		require.NoError(t, err)
		_ = execwire.WriteFrame(conn, execwire.FrameResult, payload)
	})

	cl := NewClient(socket, time.Now)
	var out, errOut strings.Builder
	code, err := cl.Exec(context.Background(), "MGIT-138",
		model.ExecRequest{Command: []string{"make"}}, &out, &errOut)

	require.Error(t, err, "a beat-less pre-handshake daemon served an exec; "+
		"the client would need the removed no-liveness fallback to survive it")
	assert.ErrorIs(t, err, model.ErrSandboxVersionSkew,
		"the refusal must be the version mismatch, settled at the handshake")
	assert.Equal(t, -1, code)
	assert.Empty(t, out.String(), "the client read the old daemon's beat-less frame stream")
	assert.NotErrorIs(t, err, model.ErrSandboxDaemonUnresponsive,
		"a build too old to beat is refused as old, never accused of stalling")
}

// TestHandshake_SkewText_CrossesTheWireIntact verifies the daemon's refusal
// text is the same single-sourced message the client builds, so an operator
// reading either side sees one answer. Refs: MGIT-136
func TestHandshake_SkewText_CrossesTheWireIntact(t *testing.T) {
	skipUnsupportedHostIPC(t)
	cfg, _ := dispatchConfig(t, &fakeDispatcher{})
	cfg.Version = "0.6.0 (commit: deadbee)"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runDaemon(ctx, t, cfg)

	conn := dialGreetedOnly(t, cfg.SocketPath)
	defer func() { _ = conn.Close() }()
	require.NoError(t, controlproto.WriteRequest(conn, &controlproto.Request{Kind: controlproto.KindList}))
	resp, err := controlproto.ReadResponse(conn)
	require.NoError(t, err)

	want := controlproto.SkewMessage(
		controlproto.Peer{Protocol: controlproto.LegacyProtocol},
		controlproto.Peer{Protocol: controlproto.ProtocolVersion, Version: "0.6.0 (commit: deadbee)"})
	assert.Equal(t, want, resp.Error)

	cancel()
	require.NoError(t, <-done)
}

// legacyDaemonSocket serves a unix socket that emits the real greeting and
// then runs handler on the connection, so a client can be driven against a
// daemon whose behavior we choose. Refs: MGIT-136
func legacyDaemonSocket(t *testing.T, handler func(net.Conn)) string {
	t.Helper()
	path := shortSocketPath(t)
	ln, err := net.Listen("unix", path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				if _, werr := conn.Write([]byte(greeting)); werr != nil {
					return
				}
				handler(conn)
			}()
		}
	}()
	return path
}

// TestHandshake_ResultFrameError_IsValidJSON guards the exec refusal's own
// encoding: an old client json.Unmarshals that payload, and a malformed one
// would turn a legible refusal back into an anonymous decode failure.
// Refs: MGIT-136
func TestHandshake_ResultFrameError_IsValidJSON(t *testing.T) {
	payload, err := json.Marshal(execRefusalFrame())
	require.NoError(t, err)
	var got execwire.ResultFrame
	require.NoError(t, json.Unmarshal(payload, &got))
	assert.Equal(t, -1, got.Result.ExitCode)
	assert.Contains(t, got.Error, model.ErrSandboxVersionSkew.Error())
}

// TestRefuseExecUnversioned_BrokenConnection_LoggedNoCrash covers the branch a
// vanished peer takes. A refusal is a courtesy to a client that is already
// failing, so a write that cannot land must end quietly and audibly rather
// than panicking the daemon that supervises every VM. Refs: MGIT-136, MGIT-11.10.8
func TestRefuseExecUnversioned_BrokenConnection_LoggedNoCrash(t *testing.T) {
	cfg, logs := dispatchConfig(t, &fakeDispatcher{})
	d, err := New(cfg)
	require.NoError(t, err)

	// Both writes fail: the message never leaves, and nothing crashes.
	d.refuseExecUnversioned(failConn{}, "some message")
	assert.NotContains(t, logs.String(), "panic")

	// Only the terminal frame fails: the stderr frame landed, so this one is
	// the audited write error.
	d.refuseExecUnversioned(&failSecondWriteConn{}, "some message")
	assert.Contains(t, logs.String(), `"write_error"`)
}

// failSecondWriteConn accepts the whole first frame (execwire writes a header
// and a payload separately, so that is two writes) and fails everything after
// it, breaking a two-frame refusal exactly between its frames.
type failSecondWriteConn struct {
	net.Conn
	writes int
}

func (c *failSecondWriteConn) Write(p []byte) (int, error) {
	c.writes++
	if c.writes > 2 {
		return 0, assert.AnError
	}
	return len(p), nil
}

func (*failSecondWriteConn) SetWriteDeadline(time.Time) error { return nil }
