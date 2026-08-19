package sandboxd

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
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
	// beats is how many liveness beats precede the reply, at beatEvery. Zero
	// makes this peer an mgit-sandboxd that PREDATES MGIT-133 — the mixed
	// -version pair a client must not mistake for a wedge.
	beats     int
	beatEvery time.Duration
	// chatterEvery, when non-zero, streams chatterCount stdout frames at that
	// interval BEFORE the reply and WITHOUT any beat. It models a peer whose
	// liveness signal is the command's own output rather than a heartbeat.
	chatterEvery time.Duration
	chatterCount int
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
		for i := 0; i < s.beats; i++ {
			if i > 0 {
				time.Sleep(s.beatEvery)
			}
			if err := execwire.WriteHeartbeat(conn); err != nil {
				return
			}
		}
		for i := 0; i < s.chatterCount; i++ {
			time.Sleep(s.chatterEvery)
			if err := execwire.WriteFrame(conn, execwire.FrameStdout, []byte("tick\n")); err != nil {
				return
			}
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

// TestClient_Exec_OlderDaemonNeverBeats_WaitsAndSaysWhy is the mixed-version
// pair, and it is the ordinary one: an upgraded CLI talking to the long-lived
// daemon the previous release left running.
//
// That daemon emits no liveness beat, ever. Reading its silence as a wedge
// would break every such pair — and the pair is not exotic, it is what an
// upgrade looks like until the daemon idles out. So the client drops the stall
// deadline, waits as MGIT-122 established, and states plainly that it has no
// signal to judge by rather than implying the wait is known to be healthy.
// Refs: MGIT-133, MGIT-122
func TestClient_Exec_OlderDaemonNeverBeats_WaitsAndSaysWhy(t *testing.T) {
	skipUnsupportedHostIPC(t)
	// No beats, and a reply well past the stall deadline: an old daemon
	// running a command that outlives the window.
	srv := &slowServer{replyDelay: 600 * time.Millisecond, execFrames: []byte("done\n"), execExit: 5}
	client := NewClient(srv.start(t), time.Now)
	client.stallTimeout = 100 * time.Millisecond

	var stdout, stderr bytes.Buffer
	code, err := client.Exec(context.Background(), "MGIT-133",
		model.ExecRequest{Command: []string{"go", "test", "./..."}}, &stdout, &stderr)

	require.NoError(t, err, "a daemon that never beats was accused of stalling; "+
		"every mixed-version pair would fail this way")
	assert.Equal(t, "done\n", stdout.String())
	assert.Equal(t, 5, code)
	assert.Contains(t, stderr.String(), "predates MGIT-133",
		"the caller was left waiting with no explanation of why nothing bounds the wait")
	assert.Equal(t, 1, strings.Count(stderr.String(), "predates MGIT-133"),
		"the notice repeated; it is a one-time statement about the daemon, not a ticker")
}

// TestClient_Exec_BeatsThenSilence_ReportsTheDaemon covers the transition the
// whole mechanism turns on: a daemon that HAS beaten has proved its silence is
// meaningful, so silence after that is a stall — not an old daemon, and not a
// slow build. Refs: MGIT-133
func TestClient_Exec_BeatsThenSilence_ReportsTheDaemon(t *testing.T) {
	skipUnsupportedHostIPC(t)
	srv := &slowServer{beats: 3, beatEvery: 30 * time.Millisecond, silent: true}
	client := NewClient(srv.start(t), time.Now)
	client.stallTimeout = 150 * time.Millisecond

	var stdout, stderr bytes.Buffer
	_, err := client.Exec(context.Background(), "MGIT-133",
		model.ExecRequest{Command: []string{"make"}}, &stdout, &stderr)

	require.ErrorIs(t, err, model.ErrSandboxDaemonUnresponsive)
	assert.NotContains(t, stderr.String(), "predates MGIT-133",
		"a daemon that beat and then stopped is wedged, not old")
}

// TestClient_Exec_Canceled_IsNotReportedAsAStall guards a trap the stall check
// walked straight into.
//
// Cancellation reaches the frame read AS an expired deadline — watchCancel
// expires the connection rather than closing it (MGIT-122) — so a client that
// reads "deadline exceeded" as a verdict about the daemon will either blame a
// healthy daemon for the user's own Ctrl-C or, worse, swallow the cancellation
// as a mixed-version fallback and wait forever. Refs: MGIT-133, MGIT-122
func TestClient_Exec_Canceled_IsNotReportedAsAStall(t *testing.T) {
	skipUnsupportedHostIPC(t)
	srv := &slowServer{silent: true} // never beats and never answers
	client := NewClient(srv.start(t), time.Now)
	client.stallTimeout = time.Hour // only cancellation can end this call

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()

	done := make(chan error, 1)
	go func() {
		_, err := client.Exec(ctx, "MGIT-133",
			model.ExecRequest{Command: []string{"/bin/sleep", "600"}}, &bytes.Buffer{}, &bytes.Buffer{})
		done <- err
	}()
	select {
	case err := <-done:
		require.Error(t, err)
		assert.ErrorIs(t, err, context.Canceled)
		assert.NotErrorIs(t, err, model.ErrSandboxDaemonUnresponsive,
			"the caller's own withdrawal was reported as a daemon stall")
	case <-time.After(5 * time.Second):
		t.Fatal("a canceled exec stayed blocked: cancellation was swallowed by the stall check")
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

// TestExec_ChattyLongCommand_OutputAloneKeepsItAlive pins that OUTPUT rearms
// the stall deadline, not only heartbeats.
//
// WHAT THIS GUARDS, stated carefully because the history was twice reported
// wrong and the second report was the wrong one. The deadline MGIT-122 removed
// was ABSOLUTE: it killed a command at 30.0s whether it was silent or emitting
// on stdout or stderr. A reading that it was per-read (idle) was relayed from
// timings alone and has since been retracted — three long "survivals" cited as
// evidence turned out to have run DETACHED, never exposed to the wall at any
// duration.
//
// So this test is NOT a regression test for old behavior. It pins the NEW
// rearming semantics MGIT-133 introduces, where the deadline bounds SILENCE and
// any frame resets it:
//
//	silent + long   dies unless BEATS arrive        (TestDaemon_Exec_SilentLongCommand…)
//	chatty + long   dies unless OUTPUT also rearms  (this test)
//
// The gap is real even though the old defect was not this shape: a change that
// rearms on heartbeats but not on output would pass the silent pin — the beats
// keep coming — while breaking any caller whose frames are its own output, and
// it would present as a heartbeat bug rather than a deadline bug.
//
// Note that on today's daemon nothing reaches the client mid-command: the guest
// leg streams into a bytes.Buffer (backend/microvm/manager.go) and the result is
// relayed only on completion (sandboxd/dispatch.go), so this shape cannot yet
// occur in production. That makes this a guard for a future streaming relay
// rather than a live invariant — and it is worth keeping precisely because the
// day that relay lands, nothing else would catch a beats-only rearm.
//
// This peer sends NO beats at all, so the only thing that can keep the stream
// alive is its output. Verified load-bearing against a mutant that arms the
// deadline once before the read loop instead of before every read.
// Refs: MGIT-133, MGIT-122, R-H269, R-H273
func TestExec_ChattyLongCommand_OutputAloneKeepsItAlive(t *testing.T) {
	// Six intervals of chatter across a window several times the stall bound,
	// with no beat in it: if only beats rearmed, this could not survive.
	const stall = 150 * time.Millisecond
	srv := &slowServer{
		chatterEvery: stall / 3,
		chatterCount: 6,
		execFrames:   []byte("done\n"),
	}
	client := NewClient(srv.start(t), time.Now)
	client.stallTimeout = stall

	var stdout, stderr bytes.Buffer
	res, err := client.Exec(context.Background(), "MGIT-133",
		model.ExecRequest{Command: []string{"build"}}, &stdout, &stderr)
	require.NoError(t, err,
		"a command whose own output kept arriving was killed anyway: the idle "+
			"deadline is being rearmed by beats only, which breaks every streaming caller")
	assert.Equal(t, 0, res, "a completed command reports its exit code")
	assert.Contains(t, stdout.String(), "done",
		"the final output must survive the chatter that preceded it")
}
