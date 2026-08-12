package microvm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/guest"
	"github.com/hyper-swe/mgit/internal/model"
)

func skipWithoutPOSIXShell(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("exec routes POSIX commands; the guest runs only inside the Linux microVM (FR-17.16)")
	}
}

// pipeDialer wires each DialGuest to a real guest.Supervisor over an
// in-memory pipe — the production wire protocol without a VM. A non-nil
// err makes the dial fail.
type pipeDialer struct {
	err    error
	calls  int
	lastID string
}

func (d *pipeDialer) DialGuest(ctx context.Context, id string) (net.Conn, error) {
	d.calls++
	d.lastID = id
	if d.err != nil {
		return nil, d.err
	}
	client, server := net.Pipe()
	sup := guest.NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)))
	go func() {
		defer func() { _ = server.Close() }()
		_ = sup.Serve(ctx, server)
	}()
	return client, nil
}

// execManager builds a manager with the given dialer (nil = exec
// transport unavailable).
func execManager(t *testing.T, dialer GuestDialer) *Manager {
	t.Helper()
	images := testImages(t)
	mgr, err := NewManager(Config{
		Backend:     model.BackendKVM,
		WorkDir:     t.TempDir(),
		Resolve:     func(string) (ImagePaths, error) { return images, nil },
		Hypervisor:  &fakeHypervisor{},
		GuestDialer: dialer,
		Logger:      slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		Clock:       func() time.Time { return time.Date(2026, 6, 17, 12, 0, 0, 0, time.UTC) },
		// Short readiness bound so the always-fail dial tests surface quickly
		// (the retry-then-succeed test overrides nothing — it just succeeds
		// within this window). Refs: MGIT-58
		GuestReadyTimeout: 300 * time.Millisecond,
	})
	require.NoError(t, err)
	return mgr
}

// TestExec_RoutesToGuest verifies a launched sandbox routes a command to
// the guest over the dialer and returns its output and exit code.
func TestExec_RoutesToGuest(t *testing.T) {
	skipWithoutPOSIXShell(t)
	dialer := &pipeDialer{}
	mgr := execManager(t, dialer)
	ctx := context.Background()
	info, err := mgr.Launch(ctx, launchOpts("MGIT-11.9.2", model.NetworkModeNone))
	require.NoError(t, err)

	res, err := mgr.Exec(ctx, info.ID, model.ExecRequest{Command: []string{"/bin/sh", "-c", "echo hi"}})
	require.NoError(t, err)
	assert.Equal(t, 0, res.ExitCode)
	assert.Equal(t, "hi\n", string(res.Stdout))
	assert.Equal(t, 1, dialer.calls)
	assert.Equal(t, info.ID, dialer.lastID, "exec dials the bound sandbox")
}

// TestExec_NoDialer_Unavailable verifies exec fails honestly (not faked)
// when no guest transport is wired.
func TestExec_NoDialer_Unavailable(t *testing.T) {
	mgr := execManager(t, nil)
	ctx := context.Background()
	info, err := mgr.Launch(ctx, launchOpts("MGIT-1", model.NetworkModeNone))
	require.NoError(t, err)
	_, err = mgr.Exec(ctx, info.ID, model.ExecRequest{Command: []string{"/bin/true"}})
	assert.ErrorIs(t, err, model.ErrSandboxBackendUnavailable)
}

// TestExec_UnknownSandbox verifies an unregistered id fails closed.
func TestExec_UnknownSandbox(t *testing.T) {
	mgr := execManager(t, &pipeDialer{})
	_, err := mgr.Exec(context.Background(), "no-such", model.ExecRequest{Command: []string{"/bin/true"}})
	assert.ErrorIs(t, err, model.ErrSandboxNotFound)
}

// TestExec_NotRunning_Unavailable verifies a suspended sandbox cannot exec.
func TestExec_NotRunning_Unavailable(t *testing.T) {
	mgr := execManager(t, &pipeDialer{})
	ctx := context.Background()
	info, err := mgr.Launch(ctx, launchOpts("MGIT-1", model.NetworkModeNone))
	require.NoError(t, err)
	require.NoError(t, mgr.Stop(ctx, info.ID, false))
	_, err = mgr.Exec(ctx, info.ID, model.ExecRequest{Command: []string{"/bin/true"}})
	assert.ErrorIs(t, err, model.ErrSandboxBackendUnavailable)
}

// TestExec_InvalidRequest verifies an invalid request is rejected before
// any dial.
func TestExec_InvalidRequest(t *testing.T) {
	dialer := &pipeDialer{}
	mgr := execManager(t, dialer)
	ctx := context.Background()
	info, err := mgr.Launch(ctx, launchOpts("MGIT-1", model.NetworkModeNone))
	require.NoError(t, err)
	_, err = mgr.Exec(ctx, info.ID, model.ExecRequest{Command: nil}) // empty command
	require.Error(t, err)
	assert.Zero(t, dialer.calls, "an invalid request never dials the guest")
}

// TestExec_DialError_Surfaces verifies a persistent dial failure surfaces
// after the readiness window (the guest never comes up).
func TestExec_DialError_Surfaces(t *testing.T) {
	mgr := execManager(t, &pipeDialer{err: errors.New("vsock refused")})
	ctx := context.Background()
	info, err := mgr.Launch(ctx, launchOpts("MGIT-1", model.NetworkModeNone))
	require.NoError(t, err)
	_, err = mgr.Exec(ctx, info.ID, model.ExecRequest{Command: []string{"/bin/true"}})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "dial guest")
	assert.Contains(t, err.Error(), "not ready", "a never-ready guest reports a readiness timeout")
}

// TestExec_RetriesUntilGuestReady is the MGIT-58 regression: the first exec
// after a lazy launch must WAIT for the guest vsock listener instead of
// EOFing on a too-early dial. A dialer that fails a few times (guest still
// booting) then succeeds must yield a working exec, not an error.
func TestExec_RetriesUntilGuestReady(t *testing.T) {
	skipWithoutPOSIXShell(t)
	dialer := &flakyDialer{failFirst: 3, inner: &pipeDialer{}}
	mgr := execManager(t, dialer)
	ctx := context.Background()
	info, err := mgr.Launch(ctx, launchOpts("MGIT-58", model.NetworkModeNone))
	require.NoError(t, err)

	res, err := mgr.Exec(ctx, info.ID, model.ExecRequest{Command: []string{"/bin/sh", "-c", "echo ready"}})
	require.NoError(t, err, "exec must wait out the guest boot, not EOF on the first dial")
	assert.Equal(t, "ready\n", string(res.Stdout))
	assert.Equal(t, 4, dialer.calls, "3 not-ready dials then the successful one")
}

// TestExec_ReadinessRespectsContextCancel verifies the readiness wait stops
// promptly when the caller's context is canceled, surfacing the last dial
// error rather than spinning to the timeout. Refs: MGIT-58
func TestExec_ReadinessRespectsContextCancel(t *testing.T) {
	mgr := execManager(t, &pipeDialer{err: errors.New("vsock refused")})
	ctx, cancel := context.WithCancel(context.Background())
	info, err := mgr.Launch(context.Background(), launchOpts("MGIT-58b", model.NetworkModeNone))
	require.NoError(t, err)
	cancel() // canceled before exec
	_, err = mgr.Exec(ctx, info.ID, model.ExecRequest{Command: []string{"/bin/true"}})
	assert.Error(t, err)
}

// flakyDialer fails its first failFirst DialGuest calls (guest still
// booting), then delegates to inner (guest ready). Models the lazy-boot
// readiness window. Refs: MGIT-58
type flakyDialer struct {
	failFirst int
	calls     int
	inner     GuestDialer
}

func (d *flakyDialer) DialGuest(ctx context.Context, id string) (net.Conn, error) {
	d.calls++
	if d.calls <= d.failFirst {
		return nil, errors.New("fcvsock: read handshake reply: EOF")
	}
	return d.inner.DialGuest(ctx, id)
}

// TestExec_GuestStartFailure_Surfaces verifies a guest-reported start
// failure (a command that cannot exec) surfaces from the manager.
func TestExec_GuestStartFailure_Surfaces(t *testing.T) {
	skipWithoutPOSIXShell(t)
	mgr := execManager(t, &pipeDialer{})
	ctx := context.Background()
	info, err := mgr.Launch(ctx, launchOpts("MGIT-1", model.NetworkModeNone))
	require.NoError(t, err)
	_, err = mgr.Exec(ctx, info.ID, model.ExecRequest{Command: []string{"/no/such/binary-xyz"}})
	assert.Error(t, err, "a guest start failure is not a silent success")
}

// TestExec_ContextDeadline_Applied verifies a context deadline is applied
// to the guest connection (the deadline branch) and a normal exec still
// completes.
func TestExec_ContextDeadline_Applied(t *testing.T) {
	skipWithoutPOSIXShell(t)
	mgr := execManager(t, &pipeDialer{})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	info, err := mgr.Launch(ctx, launchOpts("MGIT-1", model.NetworkModeNone))
	require.NoError(t, err)
	res, err := mgr.Exec(ctx, info.ID, model.ExecRequest{Command: []string{"/bin/sh", "-c", "echo ok"}})
	require.NoError(t, err)
	assert.Equal(t, "ok\n", string(res.Stdout))
}

// muteConn is a connection that accepts everything written to it and then
// returns EOF — the exact shape of libkrun's vsock while the guest is still
// booting: the host-side endpoint exists and takes the request, and nothing
// inside the guest is there to receive it.
type muteConn struct{ net.Conn }

func (muteConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (muteConn) Write(p []byte) (int, error)      { return len(p), nil }
func (muteConn) Close() error                     { return nil }
func (muteConn) SetDeadline(time.Time) error      { return nil }
func (muteConn) SetReadDeadline(time.Time) error  { return nil }
func (muteConn) SetWriteDeadline(time.Time) error { return nil }

// muteDialer hands back mute connections for its first failFirst calls, then
// delegates to inner.
//
// This is NOT what flakyDialer models. flakyDialer fails the DIAL; here the
// dial succeeds and the failure only shows up after the request has been
// sent, which is why a retry loop watching dial errors alone sails past it.
// Refs: MGIT-61.15, MGIT-58
type muteDialer struct {
	failFirst int
	calls     int
	inner     GuestDialer
}

func (d *muteDialer) DialGuest(ctx context.Context, id string) (net.Conn, error) {
	d.calls++
	if d.calls <= d.failFirst {
		return muteConn{}, nil
	}
	return d.inner.DialGuest(ctx, id)
}

// TestExec_GuestConnectionThatImmediatelyEOFs_IsRetried covers the first
// command an agent runs after `mgit work --sandbox`, which is the one that
// races the guest's boot. Before this, it failed with a bare "read frame:
// EOF" and the SECOND command succeeded — the worst possible shape for
// trust in the tool.
func TestExec_GuestConnectionThatImmediatelyEOFs_IsRetried(t *testing.T) {
	skipWithoutPOSIXShell(t)
	dialer := &muteDialer{failFirst: 2, inner: &pipeDialer{}}
	mgr := execManager(t, dialer)
	ctx := context.Background()
	info, err := mgr.Launch(ctx, launchOpts("MGIT-1", model.NetworkModeNone))
	require.NoError(t, err)

	res, err := mgr.Exec(ctx, info.ID, model.ExecRequest{Command: []string{"echo", "ready"}})

	require.NoError(t, err, "a connection that EOFs before the guest is listening must be retried")
	assert.Equal(t, 0, res.ExitCode)
	assert.Equal(t, 3, dialer.calls, "the two dead connections must each have been retried")
}

// TestExec_OnceTheGuestHasAnswered_ASilentEOFIsReported verifies the retry
// window CLOSES. A guest that dies mid-session must surface as a failure, not
// be quietly re-run — the retry is only ever safe before the guest has proven
// it can receive anything.
func TestExec_OnceTheGuestHasAnswered_ASilentEOFIsReported(t *testing.T) {
	skipWithoutPOSIXShell(t)
	// Answers the first command, then goes mute for every command after it.
	dialer := &muteAfterDialer{inner: &pipeDialer{}}
	mgr := execManager(t, dialer)
	ctx := context.Background()
	info, err := mgr.Launch(ctx, launchOpts("MGIT-1", model.NetworkModeNone))
	require.NoError(t, err)
	_, err = mgr.Exec(ctx, info.ID, model.ExecRequest{Command: []string{"echo", "one"}})
	require.NoError(t, err, "the first command must succeed for this test to mean anything")

	start := time.Now()
	_, err = mgr.Exec(ctx, info.ID, model.ExecRequest{Command: []string{"echo", "two"}})

	require.Error(t, err, "a guest that stops answering mid-session is a real failure")
	assert.Less(t, time.Since(start), time.Second,
		"it must fail immediately, not sit in the first-command retry window")
}

// muteAfterDialer answers once, then goes mute.
type muteAfterDialer struct {
	answered bool
	inner    GuestDialer
}

func (d *muteAfterDialer) DialGuest(ctx context.Context, id string) (net.Conn, error) {
	if d.answered {
		return muteConn{}, nil
	}
	d.answered = true
	return d.inner.DialGuest(ctx, id)
}

// resetConn is a connection that accepts the request and then reports the
// connection reset the VMM produces when nothing is listening on the guest
// side yet. It reads as a real socket error, wrapped exactly as the net
// package wraps it, because the predicate under test unwraps to the errno.
type resetConn struct {
	net.Conn
	errno syscall.Errno
}

func (c *resetConn) Read([]byte) (int, error) {
	return 0, &net.OpError{Op: "read", Net: "unix", Err: os.NewSyscallError("read", c.errno)}
}

func (c *resetConn) Write(b []byte) (int, error) { return len(b), nil }
func (c *resetConn) Close() error                { return nil }
func (c *resetConn) SetDeadline(time.Time) error { return nil }

// resettingDialer hands back connections that reset for the first failFirst
// calls — the libkrun window where the host-side vsock socket exists but
// mgit-guest has not bound its listener — then delegates to a working dialer.
type resettingDialer struct {
	failFirst int
	errno     syscall.Errno
	calls     int
	inner     GuestDialer
}

func (d *resettingDialer) DialGuest(ctx context.Context, id string) (net.Conn, error) {
	d.calls++
	if d.calls <= d.failFirst {
		return &resetConn{errno: d.errno}, nil
	}
	return d.inner.DialGuest(ctx, id)
}

// TestExec_RetriesWhenTheGuestResetsBeforeListening is the MGIT-91 regression.
// The dial SUCCEEDS here — that is the point, and why the existing
// MGIT-58 dial-retry could not cover it: libkrun creates the host-side vsock
// socket with the VM, so a too-early exec connects and is then reset. Before
// the fix the predicate matched only io.EOF, so this surfaced to the caller as
// "connection reset by peer" on the very first command after every launch.
// Refs: MGIT-91, MGIT-58
func TestExec_RetriesWhenTheGuestResetsBeforeListening(t *testing.T) {
	skipWithoutPOSIXShell(t)
	for _, tt := range []struct {
		name  string
		errno syscall.Errno
	}{
		{"connection_reset", syscall.ECONNRESET},
		{"broken_pipe", syscall.EPIPE},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dialer := &resettingDialer{failFirst: 2, errno: tt.errno, inner: &pipeDialer{}}
			mgr := execManager(t, dialer)
			info, err := mgr.Launch(context.Background(), launchOpts("MGIT-91", model.NetworkModeNone))
			require.NoError(t, err)

			res, err := mgr.Exec(context.Background(), info.ID,
				model.ExecRequest{Command: []string{"/bin/sh", "-c", "echo served"}})

			require.NoError(t, err, "a reset before the guest listens must be waited out, not surfaced")
			assert.Equal(t, "served\n", string(res.Stdout))
			assert.Equal(t, 3, dialer.calls, "2 resets then the successful attempt")
		})
	}
}

// TestExec_ResetAfterTheGuestAnswered_Surfaces pins the safety property the
// broadened predicate must not cost us. Once the guest has served a command,
// the first-command window is over and a reset is a REAL failure — an agent
// whose long-running build dies mid-stream has to see that, not have it
// silently retried into a second run. Refs: MGIT-91
func TestExec_ResetAfterTheGuestAnswered_Surfaces(t *testing.T) {
	skipWithoutPOSIXShell(t)
	inner := &pipeDialer{}
	dialer := &afterFirstResetDialer{inner: inner}
	mgr := execManager(t, dialer)
	info, err := mgr.Launch(context.Background(), launchOpts("MGIT-91b", model.NetworkModeNone))
	require.NoError(t, err)

	_, err = mgr.Exec(context.Background(), info.ID, model.ExecRequest{Command: []string{"/bin/sh", "-c", "echo one"}})
	require.NoError(t, err, "the first command establishes that the guest answers")

	_, err = mgr.Exec(context.Background(), info.ID, model.ExecRequest{Command: []string{"/bin/sh", "-c", "echo two"}})

	require.Error(t, err, "a reset AFTER the guest has answered must surface, never be retried away")
	assert.Contains(t, err.Error(), "exec")
}

// afterFirstResetDialer serves the first exec normally and resets every one
// after it.
type afterFirstResetDialer struct {
	calls int
	inner GuestDialer
}

func (d *afterFirstResetDialer) DialGuest(ctx context.Context, id string) (net.Conn, error) {
	d.calls++
	if d.calls == 1 {
		return d.inner.DialGuest(ctx, id)
	}
	return &resetConn{errno: syscall.ECONNRESET}, nil
}

// TestIsSilentDisconnect_OnlyWhenNothingWasSaid covers the predicate directly,
// including the case that must stay FALSE: a connection that dropped after the
// guest had already written something cannot be re-sent, because part of the
// command's effect is already real. Refs: MGIT-91
func TestIsSilentDisconnect_OnlyWhenNothingWasSaid(t *testing.T) {
	reset := &net.OpError{Op: "read", Net: "unix", Err: os.NewSyscallError("read", syscall.ECONNRESET)}
	tests := []struct {
		name           string
		err            error
		stdout, stderr []byte
		want           bool
	}{
		{"clean_eof_no_output", io.EOF, nil, nil, true},
		{"reset_no_output", reset, nil, nil, true},
		{"wrapped_reset_no_output", fmt.Errorf("read frame: %w", reset), nil, nil, true},
		{"broken_pipe_no_output", os.NewSyscallError("write", syscall.EPIPE), nil, nil, true},
		{"reset_after_stdout", reset, []byte("partial"), nil, false},
		{"reset_after_stderr", reset, nil, []byte("boom"), false},
		{"unrelated_error", errors.New("protocol violation"), nil, nil, false},
		{"nil_error", nil, nil, nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isSilentDisconnect(tt.err, tt.stdout, tt.stderr))
		})
	}
}
