// Package sandboxd exec-liveness tests cover the property MGIT-133 exists for:
// a client blocked on an exec stream can tell a guest that is working from a
// daemon that stopped answering, and says which.
//
// The two failure modes these guard against are opposites, and a fix for
// either one alone is a regression of the other. Bounding the command kills
// healthy builds (MGIT-122). Bounding nothing leaves a wedge silent forever
// (this ticket). Every test here therefore asserts one of the two halves:
// a long or silent command SURVIVES, or a stalled daemon is CAUGHT and named.
// Refs: MGIT-133, MGIT-122, MGIT-118
package sandboxd

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/controlproto"
	"github.com/hyper-swe/mgit/internal/execwire"
	"github.com/hyper-swe/mgit/internal/model"
)

// testBeatInterval compresses the daemon's beat cadence so these tests measure
// behavior rather than wall clock. The RELATIONSHIP between interval and stall
// deadline is what is under test; execwire pins the production values.
const testBeatInterval = 40 * time.Millisecond

// stallingDispatcher is a daemon that is ALIVE but progressively unable to
// answer for its own state.
//
// This is the honest shape of the wedge MGIT-133 is about, and the reason the
// beat is gated on a probe rather than free-running. Exec never returns — from
// the outside, indistinguishable from a long build. Status answers the first
// healthyStatus calls and then blocks forever, which is what a daemon
// deadlocked on the sandbox registry looks like: the process is scheduled, its
// goroutines run, and it can no longer say anything about itself.
//
// A naive implementation — a ticker goroutine writing beats on a timer — keeps
// beating straight through this and certifies a liveness that does not exist.
// TestExec_DaemonWedges_ClientNamesTheDaemon fails against such an
// implementation, which is the point of writing it this way rather than with a
// flag that tells the client to give up. Refs: MGIT-133
type stallingDispatcher struct {
	healthyStatus int64         // Status calls answered before the wedge
	statusCalls   atomic.Int64  // observed, so a test can assert the probe ran
	wedge         chan struct{} // never closed: blocking here is the wedge
}

func newStallingDispatcher(healthyStatus int64) *stallingDispatcher {
	return &stallingDispatcher{healthyStatus: healthyStatus, wedge: make(chan struct{})}
}

func (s *stallingDispatcher) Exec(_ context.Context, _ string, _ model.ExecRequest) (*model.ExecResult, error) {
	<-s.wedge // the command "is still running", forever
	return nil, nil
}

func (s *stallingDispatcher) Status(_ context.Context, taskID string) (*model.SandboxInfo, error) {
	if s.statusCalls.Add(1) > s.healthyStatus {
		<-s.wedge // the daemon can no longer answer for its own state
	}
	return &model.SandboxInfo{ID: "01JXSBSANDBOX", TaskID: taskID, State: model.StateRunning}, nil
}

func (s *stallingDispatcher) Register(_ context.Context, _ model.SandboxLaunchOptions) (*model.SandboxInfo, error) {
	return nil, model.ErrSandboxNotFound
}
func (s *stallingDispatcher) List(_ context.Context) ([]model.SandboxInfo, error) { return nil, nil }
func (s *stallingDispatcher) Remove(_ context.Context, _ string, _ bool) error    { return nil }
func (s *stallingDispatcher) SyncWorktree(_ context.Context, _ string,
	_ model.WorktreeSyncOptions) (*model.WorktreeSyncReport, error) {
	return nil, nil
}

// slowExecDispatcher is a healthy daemon running a command that takes a while
// and, optionally, says nothing at all while it does.
type slowExecDispatcher struct {
	duration time.Duration
	result   *model.ExecResult
}

func (s *slowExecDispatcher) Exec(ctx context.Context, _ string, _ model.ExecRequest) (*model.ExecResult, error) {
	select {
	case <-time.After(s.duration):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return s.result, nil
}

func (s *slowExecDispatcher) Status(_ context.Context, taskID string) (*model.SandboxInfo, error) {
	return &model.SandboxInfo{ID: "01JXSBSANDBOX", TaskID: taskID, State: model.StateRunning}, nil
}

func (s *slowExecDispatcher) Register(_ context.Context, _ model.SandboxLaunchOptions) (*model.SandboxInfo, error) {
	return nil, model.ErrSandboxNotFound
}
func (s *slowExecDispatcher) List(_ context.Context) ([]model.SandboxInfo, error) { return nil, nil }
func (s *slowExecDispatcher) Remove(_ context.Context, _ string, _ bool) error    { return nil }
func (s *slowExecDispatcher) SyncWorktree(_ context.Context, _ string,
	_ model.WorktreeSyncOptions) (*model.WorktreeSyncReport, error) {
	return nil, nil
}

// beatingDaemon starts a daemon serving svc at the compressed beat cadence and
// returns its socket path.
func beatingDaemon(ctx context.Context, t *testing.T, svc SandboxDispatcher) (string, <-chan error) {
	t.Helper()
	cfg, _ := dispatchConfig(t, svc)
	cfg.HeartbeatInterval = testBeatInterval
	done := runDaemon(ctx, t, cfg)
	// The Client dials on its own, so the socket must exist before it is
	// handed the path (dialAuthed waits, NewClient cannot).
	_ = waitForSocket(t, cfg.SocketPath).Close()
	return cfg.SocketPath, done
}

// sendExec dials the daemon, greets, and writes one exec request.
func sendExec(t *testing.T, socketPath, taskID string) net.Conn {
	t.Helper()
	conn := dialAuthed(t, socketPath)
	require.NoError(t, controlproto.WriteRequest(conn, &controlproto.Request{
		Kind: controlproto.KindExec,
		Exec: &controlproto.ExecArgs{TaskID: taskID, Exec: model.ExecRequest{Command: []string{"make"}}},
	}))
	return conn
}

// countBeats reads frames until the terminal result, returning how many
// liveness beats arrived alongside the output.
func countBeats(t *testing.T, conn net.Conn) (beats int, stdout []byte, result execwire.ResultFrame) {
	t.Helper()
	for {
		kind, payload, err := execwire.ReadFrame(conn)
		require.NoError(t, err)
		switch kind {
		case execwire.FrameHeartbeat:
			beats++
			require.Empty(t, payload, "a beat carries no payload")
		case execwire.FrameStdout:
			stdout = append(stdout, payload...)
		case execwire.FrameStderr:
		case execwire.FrameResult:
			require.NoError(t, json.Unmarshal(payload, &result))
			return beats, stdout, result
		default:
			t.Fatalf("unexpected exec frame kind %#x", kind)
		}
	}
}

// TestDaemon_Exec_SilentLongCommand_KeepsBeatingAndCompletes is MGIT-122's
// property restated as the thing the heartbeat must not break.
//
// The command runs for many beat intervals and produces its output only at the
// end — the shape of every real build, and the shape a duration-based timeout
// kills. The stream must stay alive on beats alone and deliver the output
// intact. Refs: MGIT-133, MGIT-122
func TestDaemon_Exec_SilentLongCommand_KeepsBeatingAndCompletes(t *testing.T) {
	skipUnsupportedHostIPC(t)
	svc := &slowExecDispatcher{
		duration: 6 * testBeatInterval,
		result:   &model.ExecResult{Stdout: []byte("built\n"), ExitCode: 0},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	socket, done := beatingDaemon(ctx, t, svc)

	conn := sendExec(t, socket, "MGIT-133")
	defer func() { _ = conn.Close() }()
	require.NoError(t, conn.SetDeadline(time.Now().Add(10*time.Second)))

	beats, stdout, result := countBeats(t, conn)
	assert.Equal(t, "built\n", string(stdout), "a command silent for its whole run still delivered its output")
	assert.Equal(t, 0, result.Result.ExitCode)
	assert.GreaterOrEqual(t, beats, 3,
		"a command that says nothing for six beat intervals produced no liveness signal: "+
			"the client would have nothing to distinguish it from a wedge")

	cancel()
	require.NoError(t, <-done)
}

// TestDaemon_Exec_FirstFrameIsABeat pins what makes the client's very first
// idle window judgeable.
//
// The first beat goes out BEFORE the service is called — before any lazy boot,
// before any guest work — so a client that got past the handshake is owed one
// immediately, whatever the command is about to do. That is the fact MGIT-138
// leaned on when it deleted the client's "no beat yet, so perhaps this daemon
// is simply old" escape hatch: if this frame stopped being first, silence in
// the first window would stop meaning a wedge.
// Refs: MGIT-138, MGIT-133
func TestDaemon_Exec_FirstFrameIsABeat(t *testing.T) {
	skipUnsupportedHostIPC(t)
	svc := &slowExecDispatcher{duration: time.Millisecond, result: &model.ExecResult{}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	socket, done := beatingDaemon(ctx, t, svc)

	conn := sendExec(t, socket, "MGIT-133")
	defer func() { _ = conn.Close() }()
	require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))

	kind, payload, err := execwire.ReadFrame(conn)
	require.NoError(t, err)
	assert.Equal(t, byte(execwire.FrameHeartbeat), kind,
		"the exec stream must open with a beat; without it a client cannot tell a "+
			"daemon still setting up from one that has wedged")
	assert.Empty(t, payload)

	cancel()
	require.NoError(t, <-done)
}

// TestDaemon_Exec_CannotAnswerForItself_StopsBeating is the honesty test, and
// the reason the beat is gated on a probe.
//
// The daemon here is alive: its goroutines run, its ticker fires, its socket is
// writable. What it cannot do is answer for its own state — the shape a
// registry deadlock takes. A beat emitted by a goroutine that survives such a
// wedge would be WORSE than no beat, because it certifies a liveness that does
// not exist. So the beats must stop, and stop quickly: at most one may follow
// the last real answer. Refs: MGIT-133
func TestDaemon_Exec_CannotAnswerForItself_StopsBeating(t *testing.T) {
	skipUnsupportedHostIPC(t)
	svc := newStallingDispatcher(3) // three healthy probes, then the wedge
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	socket, done := beatingDaemon(ctx, t, svc)

	conn := sendExec(t, socket, "MGIT-133")
	defer func() { _ = conn.Close() }()

	// Read for well past the point a free-running ticker would have produced a
	// beat, counting what actually arrives.
	beats := 0
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(20*testBeatInterval)))
	for {
		kind, _, err := execwire.ReadFrame(conn)
		if err != nil {
			break // the stream went silent, which is the expected outcome
		}
		require.Equal(t, byte(execwire.FrameHeartbeat), kind,
			"a wedged exec produced output it never ran")
		beats++
		require.Less(t, beats, 15,
			"the daemon kept beating through a wedge it could not answer for: "+
				"the beat is not gated on anything real")
	}
	assert.Positive(t, beats, "the daemon never beat at all, so nothing was under test")
	assert.LessOrEqual(t, beats, 6,
		"beats outlived the daemon's ability to answer for itself by more than the "+
			"one stale answer the gate permits")
	assert.Positive(t, svc.statusCalls.Load(), "no probe ran; the beats were free-running")

	cancel()
	<-done
}

// TestExec_DaemonWedges_ClientNamesTheDaemon is the acceptance criterion,
// end to end: a real daemon that accepts an exec and then stops answering,
// a real client, and a diagnosis that points at the daemon.
//
// The message is asserted as carefully as the error, because being right and
// being read as right are different things here. Every other mid-flight exec
// failure means the guest, and MGIT-118 is what a daemon-side failure dressed
// in guest clothing costs: an agent told its build exhausted guest memory
// reshaped a build that was never the problem. Refs: MGIT-133, MGIT-118
func TestExec_DaemonWedges_ClientNamesTheDaemon(t *testing.T) {
	skipUnsupportedHostIPC(t)
	svc := newStallingDispatcher(2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	socket, done := beatingDaemon(ctx, t, svc)

	client := NewClient(socket, time.Now)
	client.stallTimeout = 4 * testBeatInterval

	var stdout, stderr bytes.Buffer
	errCh := make(chan error, 1)
	go func() {
		_, err := client.Exec(context.Background(), "MGIT-133",
			model.ExecRequest{Command: []string{"make"}}, &stdout, &stderr)
		errCh <- err
	}()

	var err error
	select {
	case err = <-errCh:
	case <-time.After(10 * time.Second):
		t.Fatal("the client waited indefinitely on a daemon that stopped answering: " +
			"a wedge is still indistinguishable from a long build")
	}

	require.Error(t, err)
	require.ErrorIs(t, err, model.ErrSandboxDaemonUnresponsive,
		"a daemon-side stall must be a distinct sentinel; anything else is diagnosed as a guest failure")
	msg := err.Error()
	assert.Contains(t, msg, "DAEMON", "the message does not name the daemon as the suspect")
	assert.Contains(t, msg, "liveness beat")
	assert.NotContains(t, strings.ToLower(msg), "out of memory",
		"a daemon stall must never be reported as in-guest memory exhaustion (MGIT-118)")
	assert.Empty(t, stderr.String(),
		"a wedge is an error; nothing is written over the caller's own stream")

	cancel()
	<-done
}

// TestExec_LongCommandThroughRealDaemon_Survives is the other half of the same
// end-to-end path: the client must NOT accuse a daemon that keeps beating,
// however long the command runs and however little it says.
//
// This is the regression that matters most. A stall check that also kills long
// builds has reintroduced MGIT-122 with a bigger number. Refs: MGIT-133, MGIT-122
func TestExec_LongCommandThroughRealDaemon_Survives(t *testing.T) {
	skipUnsupportedHostIPC(t)
	svc := &slowExecDispatcher{
		duration: 10 * testBeatInterval, // several stall windows long
		result:   &model.ExecResult{Stdout: []byte("done\n"), ExitCode: 3},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	socket, done := beatingDaemon(ctx, t, svc)

	client := NewClient(socket, time.Now)
	client.stallTimeout = 3 * testBeatInterval

	var stdout, stderr bytes.Buffer
	code, err := client.Exec(context.Background(), "MGIT-133",
		model.ExecRequest{Command: []string{"go", "build", "./..."}}, &stdout, &stderr)
	require.NoError(t, err, "a healthy long command was killed by the stall check")
	assert.Equal(t, "done\n", stdout.String(), "the command's output is the assertion, not its exit code")
	assert.Equal(t, 3, code)
	assert.Empty(t, stderr.String(), "a beating daemon needs no advisory")

	cancel()
	require.NoError(t, <-done)
}

// TestDaemon_Exec_ClientHangsUpMidCommand_DaemonSurvives covers the ordinary
// end of a beat: the caller pressed Ctrl-C.
//
// The beat is the first thing to notice, because it is the only thing writing
// while a command runs. It must read that as "stop", not as an incident — and
// must not leave the daemon wedged on a dead socket, which would turn one
// abandoned exec into the outage this whole ticket is about. Refs: MGIT-133
func TestDaemon_Exec_ClientHangsUpMidCommand_DaemonSurvives(t *testing.T) {
	skipUnsupportedHostIPC(t)
	svc := &slowExecDispatcher{duration: time.Hour, result: &model.ExecResult{}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	socket, done := beatingDaemon(ctx, t, svc)

	conn := sendExec(t, socket, "MGIT-133")
	kind, _, err := execwire.ReadFrame(conn)
	require.NoError(t, err)
	require.Equal(t, byte(execwire.FrameHeartbeat), kind)
	require.NoError(t, conn.Close()) // the caller gives up

	// The daemon still serves: the abandoned exec did not take it with it.
	require.Eventually(t, func() bool {
		next, dErr := net.Dial("unix", socket)
		if dErr != nil {
			return false
		}
		_ = next.Close()
		return true
	}, 5*time.Second, 20*time.Millisecond, "the daemon stopped accepting after a client hung up mid-exec")

	next := dialAuthed(t, socket)
	defer func() { _ = next.Close() }()
	require.NoError(t, controlproto.WriteRequest(next, &controlproto.Request{Kind: controlproto.KindList}))
	_, err = controlproto.ReadResponse(next)
	require.NoError(t, err, "the daemon wedged on a socket its client had closed")

	cancel()
	<-done
}

// TestDaemon_Exec_ServicePanics_DaemonSurvives guards the goroutine the beat
// loop required.
//
// The service call moved off the handler goroutine so the daemon can beat
// while it runs, and handleConn's recover does not reach a goroutine it merely
// spawned — an unguarded panic there would take the whole daemon down and
// strand every running VM. Refs: MGIT-133, MGIT-11.10.8
func TestDaemon_Exec_ServicePanics_DaemonSurvives(t *testing.T) {
	skipUnsupportedHostIPC(t)
	svc := &fakeDispatcher{panicOn: "exec"}
	cfg, _ := dispatchConfig(t, svc)
	cfg.HeartbeatInterval = testBeatInterval
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runDaemon(ctx, t, cfg)

	conn := sendExec(t, cfg.SocketPath, "MGIT-133")
	require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))
	_, _, result := countBeats(t, conn)
	assert.NotEmpty(t, result.Error, "a panicking exec must come back as a result frame, not a dead daemon")
	_ = conn.Close()

	// The daemon is still serving: a second request is answered.
	next := dialAuthed(t, cfg.SocketPath)
	defer func() { _ = next.Close() }()
	require.NoError(t, controlproto.WriteRequest(next, &controlproto.Request{Kind: controlproto.KindList}))
	_, err := controlproto.ReadResponse(next)
	require.NoError(t, err, "the daemon died with the panicking exec goroutine")

	cancel()
	require.NoError(t, <-done)
}
