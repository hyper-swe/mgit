package sandboxd

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/controlproto"
	"github.com/hyper-swe/mgit/internal/execwire"
	"github.com/hyper-swe/mgit/internal/model"
)

// slowServer is a hand-rolled control-plane peer whose TIMING the test owns:
// how long it waits before greeting, and how long it waits before answering.
//
// The real daemon is used everywhere else in this package, and should be — but
// what these tests are about is which instants the client's socket deadlines
// are measured from, and that cannot be driven through a daemon that answers as
// fast as it can. Refs: MGIT-122
type slowServer struct {
	greetDelay time.Duration
	replyDelay time.Duration
	// silent leaves the request unanswered forever, so the only thing that can
	// end the call is the caller's own cancellation.
	silent bool
	// execFrames replies to an exec request with this output, framed, instead
	// of a control response.
	execFrames []byte
	execExit   int
}

// start serves exactly one connection and returns its socket path.
func (s *slowServer) start(t *testing.T) string {
	t.Helper()
	// os.MkdirTemp, not t.TempDir: the latter embeds the test name, and a unix
	// socket path is capped near 104 bytes on macOS.
	dir, err := os.MkdirTemp("", "sbd")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		time.Sleep(s.greetDelay)
		if _, err := conn.Write([]byte(greeting)); err != nil {
			return
		}
		req, err := controlproto.ReadRequest(conn)
		if err != nil {
			return
		}
		if s.silent {
			<-make(chan struct{}) // never answers; the client must cancel itself
		}
		time.Sleep(s.replyDelay)
		if req.Kind == controlproto.KindExec {
			_ = execwire.WriteFrame(conn, execwire.FrameStdout, s.execFrames)
			payload := []byte(`{"outcome":{"exit_code":` + itoa(s.execExit) + `}}`)
			_ = execwire.WriteFrame(conn, execwire.FrameResult, payload)
			return
		}
		_ = controlproto.WriteResponse(conn, &controlproto.Response{
			List: []model.SandboxInfo{{ID: "01J", TaskID: "MGIT-122"}},
		})
	}()
	return path
}

// itoa renders a small non-negative int without pulling in strconv semantics
// the test does not need.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf []byte
	for n > 0 {
		buf = append([]byte{byte('0' + n%10)}, buf...)
		n /= 10
	}
	return string(buf)
}

// TestClient_RoundTrip_ResponseBudgetStartsAtTheRequest verifies a request's
// read budget is measured from when the request was WRITTEN, not from when the
// connection was dialed.
//
// The client used to arm one read deadline at dial and never re-arm it, so
// anything that happened before the request went out — here, a daemon slow to
// greet — was silently subtracted from the time the response was allowed to
// take. The peer greets late and then answers well inside the budget: only a
// re-armed deadline sees that as the healthy exchange it is. Refs: MGIT-122
func TestClient_RoundTrip_ResponseBudgetStartsAtTheRequest(t *testing.T) {
	skipUnsupportedHostIPC(t)
	srv := &slowServer{greetDelay: 400 * time.Millisecond, replyDelay: 300 * time.Millisecond}
	client := NewClient(srv.start(t), time.Now)
	client.requestTimeout = 500 * time.Millisecond

	list, err := client.List(context.Background())
	require.NoError(t, err, "the response arrived 300ms after a request with a 500ms budget, "+
		"and was still rejected — the deadline was inherited from dial time")
	require.Len(t, list, 1)
	assert.Equal(t, "MGIT-122", list[0].TaskID)
}

// TestClient_Exec_OutlivesTheRequestTimeout_Completes is the defect that made
// the sandbox unusable for the work it exists to contain.
//
// The exec stream carries no frame until the guest command FINISHES (the
// daemon buffers the result and then relays it), so a read deadline armed at
// dial bounds the whole command. Every exec longer than the control-plane
// timeout died — measured on a warm single-VM sandbox, `sleep 45` failed at
// exactly 30 s — and the failure was rendered as in-guest memory exhaustion.
// A command's duration is the daemon's and the guest's to bound (the per-exec
// timeout and the sandbox TTL), never the control socket's. Refs: MGIT-122
func TestClient_Exec_OutlivesTheRequestTimeout_Completes(t *testing.T) {
	skipUnsupportedHostIPC(t)
	srv := &slowServer{replyDelay: 400 * time.Millisecond, execFrames: []byte("done\n"), execExit: 7}
	client := NewClient(srv.start(t), time.Now)
	client.requestTimeout = 100 * time.Millisecond

	var stdout, stderr bytes.Buffer
	code, err := client.Exec(context.Background(), "MGIT-122",
		model.ExecRequest{Command: []string{"/bin/sleep", "45"}}, &stdout, &stderr)
	require.NoError(t, err, "a command that outran the control-plane request timeout was killed by it")
	assert.Equal(t, 7, code)
	assert.Equal(t, "done\n", stdout.String())
}

// TestClient_Exec_ContextCanceled_ReturnsPromptly is the other half of removing
// the wall clock from the exec stream: with no deadline bounding it, the
// CALLER must be able to end the wait.
//
// Cancellation was previously not wired to the connection at all — a Ctrl-C
// left the client blocked in a socket read until the inherited deadline fired.
// Refs: MGIT-122
func TestClient_Exec_ContextCanceled_ReturnsPromptly(t *testing.T) {
	skipUnsupportedHostIPC(t)
	srv := &slowServer{silent: true}
	client := NewClient(srv.start(t), time.Now)

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(150 * time.Millisecond); cancel() }()

	done := make(chan error, 1)
	go func() {
		_, err := client.Exec(ctx, "MGIT-122",
			model.ExecRequest{Command: []string{"/bin/sleep", "600"}}, &bytes.Buffer{}, &bytes.Buffer{})
		done <- err
	}()
	select {
	case err := <-done:
		require.Error(t, err, "a canceled exec reported success")
	case <-time.After(5 * time.Second):
		t.Fatal("a canceled exec stayed blocked in the frame read: cancellation is not wired to the connection")
	}
}

// TestClient_RoundTrip_ContextCanceled_ReturnsPromptly asserts the same for the
// non-streaming verbs: a caller that gives up is never held by the socket.
// Refs: MGIT-122
func TestClient_RoundTrip_ContextCanceled_ReturnsPromptly(t *testing.T) {
	skipUnsupportedHostIPC(t)
	srv := &slowServer{silent: true}
	client := NewClient(srv.start(t), time.Now)
	client.requestTimeout = time.Hour // only cancellation can end this call

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(150 * time.Millisecond); cancel() }()

	done := make(chan error, 1)
	go func() {
		_, err := client.List(ctx)
		done <- err
	}()
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("a canceled round trip stayed blocked in the response read")
	}
}
