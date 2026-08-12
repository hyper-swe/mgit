package microvm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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
	assert.Equal(t, 2, dialer.calls, "launch's readiness probe, then the command itself (MGIT-92)")
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
	afterLaunch := dialer.calls                                      // launch's readiness probe dials once (MGIT-92)
	_, err = mgr.Exec(ctx, info.ID, model.ExecRequest{Command: nil}) // empty command
	require.Error(t, err)
	assert.Equal(t, afterLaunch, dialer.calls, "an invalid request never dials the guest")
}

// TestExec_DialError_Surfaces verifies a persistent dial failure surfaces
// after the readiness window (the guest never comes up).
// (MGIT-92: these two launch with a WORKING dialer and break it afterwards.
// A permanently-failing dialer can no longer produce a launched sandbox at
// all — that is the point of the ticket — so a test about Exec's behavior has
// to get past the launch first.)
func TestExec_DialError_Surfaces(t *testing.T) {
	dialer := &breakableDialer{inner: &pipeDialer{}}
	mgr := execManager(t, dialer)
	ctx := context.Background()
	info, err := mgr.Launch(ctx, launchOpts("MGIT-1", model.NetworkModeNone))
	require.NoError(t, err)
	dialer.goAway(errors.New("vsock refused")) // the guest goes away after launch

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
	dialer := &flakyDialer{inner: &pipeDialer{}}
	mgr := execManager(t, dialer)
	ctx := context.Background()
	info, err := mgr.Launch(ctx, launchOpts("MGIT-58", model.NetworkModeNone))
	require.NoError(t, err)

	// Arm the not-ready window AFTER launch. Launch now confirms the guest is
	// serving before it returns (MGIT-92), so a dialer armed up front would be
	// exhausted by that confirmation and this would no longer exercise Exec's
	// own wait. Arming here keeps the MGIT-58 regression pointed at Exec.
	dialer.armFailures(3)

	res, err := mgr.Exec(ctx, info.ID, model.ExecRequest{Command: []string{"/bin/sh", "-c", "echo ready"}})
	require.NoError(t, err, "exec must wait out the guest boot, not EOF on the first dial")
	assert.Equal(t, "ready\n", string(res.Stdout))
	assert.Equal(t, 4, dialer.callsSinceArmed(), "3 not-ready dials then the successful one")
}

// TestExec_ReadinessRespectsContextCancel verifies the readiness wait stops
// promptly when the caller's context is canceled, surfacing the last dial
// error rather than spinning to the timeout. Refs: MGIT-58
func TestExec_ReadinessRespectsContextCancel(t *testing.T) {
	dialer := &breakableDialer{inner: &pipeDialer{}}
	mgr := execManager(t, dialer)
	ctx, cancel := context.WithCancel(context.Background())
	info, err := mgr.Launch(context.Background(), launchOpts("MGIT-58b", model.NetworkModeNone))
	require.NoError(t, err)
	dialer.goAway(errors.New("vsock refused"))
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
	armed     int // calls already made when the failure window was armed
	inner     GuestDialer
}

// armFailures makes the next n dials fail, counting from now. Refs: MGIT-92
func (d *flakyDialer) armFailures(n int) {
	d.armed, d.failFirst = d.calls, d.calls+n
}

func (d *flakyDialer) callsSinceArmed() int { return d.calls - d.armed }

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
	armed     int
	inner     GuestDialer
}

func (d *muteDialer) DialGuest(ctx context.Context, id string) (net.Conn, error) {
	d.calls++
	if d.calls <= d.failFirst {
		return muteConn{}, nil
	}
	return d.inner.DialGuest(ctx, id)
}

// armFailures makes the next n dials answer with a mute connection, counting
// from now, so a test can target Exec rather than launch's readiness probe.
// Refs: MGIT-92
func (d *muteDialer) armFailures(n int) {
	d.armed, d.failFirst = d.calls, d.calls+n
}

func (d *muteDialer) callsSinceArmed() int { return d.calls - d.armed }

// TestExec_GuestConnectionThatImmediatelyEOFs_IsRetried covers the first
// command an agent runs after `mgit work --sandbox`, which is the one that
// races the guest's boot. Before this, it failed with a bare "read frame:
// EOF" and the SECOND command succeeded — the worst possible shape for
// trust in the tool.
func TestExec_GuestConnectionThatImmediatelyEOFs_IsRetried(t *testing.T) {
	skipWithoutPOSIXShell(t)
	dialer := &muteDialer{inner: &pipeDialer{}}
	mgr := execManager(t, dialer)
	ctx := context.Background()
	info, err := mgr.Launch(ctx, launchOpts("MGIT-1", model.NetworkModeNone))
	require.NoError(t, err)
	dialer.armFailures(2) // after launch's readiness probe — see armFailures

	res, err := mgr.Exec(ctx, info.ID, model.ExecRequest{Command: []string{"echo", "ready"}})

	require.NoError(t, err, "a connection that EOFs before the guest is listening must be retried")
	assert.Equal(t, 0, res.ExitCode)
	assert.Equal(t, 3, dialer.callsSinceArmed(), "the two dead connections must each have been retried")
}

// TestExec_OnceTheGuestHasAnswered_ASilentEOFIsReported verifies the retry
// window CLOSES. A guest that dies mid-session must surface as a failure, not
// be quietly re-run — the retry is only ever safe before the guest has proven
// it can receive anything.
func TestExec_OnceTheGuestHasAnswered_ASilentEOFIsReported(t *testing.T) {
	skipWithoutPOSIXShell(t)
	// Answers the first command, then goes mute for every command after it.
	// Two answers: launch's readiness probe, then the first real command. It
	// goes mute after that, which is the case under test.
	dialer := &muteAfterDialer{answers: 2, inner: &pipeDialer{}}
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
	answers int // how many dials still answer before it goes mute
	inner   GuestDialer
}

func (d *muteAfterDialer) DialGuest(ctx context.Context, id string) (net.Conn, error) {
	if d.answers <= 0 {
		return muteConn{}, nil
	}
	d.answers--
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
	armed     int
	inner     GuestDialer
}

// armResets makes the next n connections reset, counting from now. Refs: MGIT-92
func (d *resettingDialer) armResets(n int) {
	d.armed, d.failFirst = d.calls, d.calls+n
}

func (d *resettingDialer) callsSinceArmed() int { return d.calls - d.armed }

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
			dialer := &resettingDialer{errno: tt.errno, inner: &pipeDialer{}}
			mgr := execManager(t, dialer)
			info, err := mgr.Launch(context.Background(), launchOpts("MGIT-91", model.NetworkModeNone))
			require.NoError(t, err)

			// Arm the reset window AFTER launch — see armFailures. That the
			// window can still be armed at all is the property MGIT-92 had to
			// preserve: confirming readiness at launch must not disarm Exec's
			// first-command retry. TestLaunch_ReadinessProbe_LeavesTheFirstCommandRetryArmed
			// pins that directly.
			dialer.armResets(2)

			res, err := mgr.Exec(context.Background(), info.ID,
				model.ExecRequest{Command: []string{"/bin/sh", "-c", "echo served"}})

			require.NoError(t, err, "a reset before the guest listens must be waited out, not surfaced")
			assert.Equal(t, "served\n", string(res.Stdout))
			assert.Equal(t, 3, dialer.callsSinceArmed(), "2 resets then the successful attempt")
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
	dialer := &afterFirstResetDialer{serveUpTo: 1, inner: inner}
	mgr := execManager(t, dialer)
	info, err := mgr.Launch(context.Background(), launchOpts("MGIT-91b", model.NetworkModeNone))
	require.NoError(t, err)
	dialer.serveNextThenReset() // launch's readiness probe already used a dial

	_, err = mgr.Exec(context.Background(), info.ID, model.ExecRequest{Command: []string{"/bin/sh", "-c", "echo one"}})
	require.NoError(t, err, "the first command establishes that the guest answers")

	_, err = mgr.Exec(context.Background(), info.ID, model.ExecRequest{Command: []string{"/bin/sh", "-c", "echo two"}})

	require.Error(t, err, "a reset AFTER the guest has answered must surface, never be retried away")
	assert.Contains(t, err.Error(), "exec")
}

// afterFirstResetDialer serves the first exec normally and resets every one
// after it.
type afterFirstResetDialer struct {
	calls     int
	serveUpTo int
	inner     GuestDialer
}

// serveNextThenReset serves one more dial normally and resets every one after
// it, counting from now. Refs: MGIT-92
func (d *afterFirstResetDialer) serveNextThenReset() { d.serveUpTo = d.calls + 1 }

func (d *afterFirstResetDialer) DialGuest(ctx context.Context, id string) (net.Conn, error) {
	d.calls++
	if d.calls <= d.serveUpTo {
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

// consoleVM is a fakeVM that also captures a guest console, like the libkrun
// and firecracker VMs do. The console holds the guest's OWN startup error,
// which is the payload a fail-closed launch has to surface.
type consoleVM struct {
	fakeVM
	console string
}

func (v *consoleVM) ConsoleTail(maxBytes int) string {
	if len(v.console) > maxBytes {
		return v.console[len(v.console)-maxBytes:]
	}
	return v.console
}

// consoleHypervisor hands out consoleVMs preloaded with a console transcript.
type consoleHypervisor struct {
	console string
	vms     []*consoleVM
}

func (h *consoleHypervisor) CreateVM(VMConfig) (VM, error) {
	vm := &consoleVM{console: h.console}
	h.vms = append(h.vms, vm)
	return vm, nil
}

// deadGuestDialer models the failure this ticket exists for: the VMM is up, so
// the host-side socket connects, but nothing behind it ever answers.
type deadGuestDialer struct{ calls int }

func (d *deadGuestDialer) DialGuest(context.Context, string) (net.Conn, error) {
	d.calls++
	return &resetConn{errno: syscall.ECONNRESET}, nil
}

// launchManagerWithConsole builds a manager whose VMs carry the given console
// transcript, with a readiness window short enough to keep the test quick.
func launchManagerWithConsole(t *testing.T, console string, dialer GuestDialer) (*Manager, *consoleHypervisor) {
	t.Helper()
	images := testImages(t)
	hv := &consoleHypervisor{console: console}
	mgr, err := NewManager(Config{
		Backend:           model.BackendKVM,
		WorkDir:           t.TempDir(),
		Resolve:           func(string) (ImagePaths, error) { return images, nil },
		Hypervisor:        hv,
		GuestDialer:       dialer,
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		Clock:             func() time.Time { return time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC) },
		GuestReadyTimeout: 200 * time.Millisecond,
	})
	require.NoError(t, err)
	return mgr, hv
}

// TestLaunch_GuestNeverServes_FailsClosedWithTheGuestsOwnError is the MGIT-92
// regression, and it is deliberately strict about WHICH error comes back.
//
// The failure being fixed is not "launch returned success where it should have
// returned an error" alone — it is that the operator was told containment was
// established and then, later, handed a socket path. So this asserts the error
// carries the GUEST's own words, taken from the console where they already
// were. An implementation that failed the launch with a transport message
// would satisfy "returns an error" and still leave the operator exactly as
// stuck as MGIT-89 left them for weeks.
// Refs: MGIT-92, MGIT-89, NFR-17.6
func TestLaunch_GuestNeverServes_FailsClosedWithTheGuestsOwnError(t *testing.T) {
	const guestSaidWhy = "mgit-guest: write /etc/resolv.conf: operation not supported"
	console := "libkrun vm entering\n" + guestSaidWhy + "\nmgit-guest exited with error\n"
	dialer := &deadGuestDialer{}
	mgr, hv := launchManagerWithConsole(t, console, dialer)

	info, err := mgr.Launch(context.Background(), launchOpts("MGIT-92", model.NetworkModeNone))

	require.Error(t, err, "a guest that never answers must not be reported as launched")
	assert.Nil(t, info)
	assert.ErrorIs(t, err, model.ErrGuestNotServing)
	assert.Contains(t, err.Error(), guestSaidWhy,
		"the error must carry the guest's OWN failure, not just a transport symptom")

	// Fail CLOSED: nothing is left running or registered for someone to use.
	require.Len(t, hv.vms, 1)
	assert.True(t, hv.vms[0].stopped, "the VM must be torn down, not left running")
	list, err := mgr.List(context.Background())
	require.NoError(t, err)
	assert.Empty(t, list, "a sandbox that failed to come up must not be listed as one")
}

// TestLaunch_FailClosed_ReadsTheConsoleBeforeTeardownRemovesIt pins the
// ordering that makes the diagnosis possible at all: teardown deletes the
// per-sandbox state dir, and the console log lives inside it. Reading it after
// the teardown would produce a perfectly correct, perfectly useless error.
// Refs: MGIT-92
func TestLaunch_FailClosed_ReadsTheConsoleBeforeTeardownRemovesIt(t *testing.T) {
	dir := t.TempDir()
	images := testImages(t)
	hv := &realFileConsoleHypervisor{stateRoot: dir}
	mgr, err := NewManager(Config{
		Backend: model.BackendKVM, WorkDir: dir,
		Resolve:    func(string) (ImagePaths, error) { return images, nil },
		Hypervisor: hv, GuestDialer: &deadGuestDialer{},
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		Clock:             func() time.Time { return time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC) },
		GuestReadyTimeout: 200 * time.Millisecond,
	})
	require.NoError(t, err)

	_, err = mgr.Launch(context.Background(), launchOpts("MGIT-92b", model.NetworkModeNone))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "guest panicked on a real file",
		"the console must be read while it still exists")
	assert.NoFileExists(t, hv.written, "and the state dir is still cleaned up afterwards")
}

// realFileConsoleHypervisor writes a console log into the sandbox state dir,
// so the tail is read from a real file that teardown really deletes.
type realFileConsoleHypervisor struct {
	stateRoot string
	written   string
}

func (h *realFileConsoleHypervisor) CreateVM(cfg VMConfig) (VM, error) {
	path := filepath.Join(cfg.StateDir, "console.log")
	if err := os.WriteFile(path, []byte("guest panicked on a real file\n"), 0o600); err != nil {
		return nil, err
	}
	h.written = path
	return &fileConsoleVM{path: path}, nil
}

type fileConsoleVM struct {
	fakeVM
	path string
}

func (v *fileConsoleVM) ConsoleTail(maxBytes int) string { return TailFile(v.path, maxBytes) }

// TestLaunch_ReadinessProbe_LeavesTheFirstCommandRetryArmed is the guard
// against fixing MGIT-92 by breaking MGIT-91.
//
// Launch now proves the guest answers before returning, and it would be easy to
// conclude from that that the first-command retry is no longer needed and mark
// the guest "answered" at launch. That would be wrong: the probe is answered by
// a lookup failure on the read path, which is weaker evidence than a real
// command round trip, and disarming the retry would put back exactly the reset
// MGIT-91 removed. The retry window must still be open for the caller's first
// real command.
// Refs: MGIT-92, MGIT-91
func TestLaunch_ReadinessProbe_LeavesTheFirstCommandRetryArmed(t *testing.T) {
	skipWithoutPOSIXShell(t)
	dialer := &resettingDialer{errno: syscall.ECONNRESET, inner: &pipeDialer{}}
	mgr := execManager(t, dialer)
	info, err := mgr.Launch(context.Background(), launchOpts("MGIT-92c", model.NetworkModeNone))
	require.NoError(t, err, "the probe is answered, so the launch stands")

	// The guest resets the very next connection — the shape MGIT-91 fixed.
	dialer.armResets(1)
	res, err := mgr.Exec(context.Background(), info.ID,
		model.ExecRequest{Command: []string{"/bin/sh", "-c", "echo still-retried"}})

	require.NoError(t, err, "the first command's retry must survive the launch probe")
	assert.Equal(t, "still-retried\n", string(res.Stdout))
}

// TestLaunch_NoGuestDialer_SkipsConfirmation verifies the wait asserts only a
// capability the backend actually has. With no exec transport wired there is no
// control plane to confirm, and Exec already reports that honestly rather than
// faking success — so a launch must not fail on a channel that was never
// claimed to exist. Refs: MGIT-92, FR-17.16
func TestLaunch_NoGuestDialer_SkipsConfirmation(t *testing.T) {
	mgr := execManager(t, nil)
	info, err := mgr.Launch(context.Background(), launchOpts("MGIT-92d", model.NetworkModeNone))
	require.NoError(t, err, "a backend with no exec transport still launches")
	require.NotNil(t, info)
}

// TestConsoleDiagnosis_BackendWithoutAConsole_SaysSo verifies the failure
// message never contains a silent blank where the diagnosis belongs: vzf
// captures no console, and the error should say that rather than trail off.
// Refs: MGIT-92
func TestConsoleDiagnosis_BackendWithoutAConsole_SaysSo(t *testing.T) {
	assert.Contains(t, consoleDiagnosis(&fakeVM{}), "not captured by this backend")
	assert.Contains(t, consoleDiagnosis(&consoleVM{console: "   "}), "empty")
	assert.Contains(t, consoleDiagnosis(&consoleVM{console: "boom"}), "boom")
}

// TestTailFile_LongConsole_KeepsTheEnd verifies the tail is bounded and keeps
// the END of the log, where a startup failure is written. Refs: MGIT-92
func TestTailFile_LongConsole_KeepsTheEnd(t *testing.T) {
	path := filepath.Join(t.TempDir(), "console.log")
	require.NoError(t, os.WriteFile(path, []byte(strings.Repeat("x", 5000)+"THE-ACTUAL-ERROR"), 0o600))

	tail := TailFile(path, 100)

	assert.Len(t, tail, 100)
	assert.Contains(t, tail, "THE-ACTUAL-ERROR")
	assert.Empty(t, TailFile(filepath.Join(t.TempDir(), "absent.log"), 100),
		"a missing console reads as no console, not as a failure")
}

// unreachableDialer models a guest that is not there at all: the dial itself
// fails, which is what the host sees once a guest process has exited.
type unreachableDialer struct{ calls int }

func (d *unreachableDialer) DialGuest(context.Context, string) (net.Conn, error) {
	d.calls++
	return nil, errors.New("vsock: connect: connection refused")
}

// TestLaunch_GuestUnreachable_FailsClosed is the regression for a mistake this
// ticket's own first implementation made, and which only real hardware caught.
//
// Readiness was decided by "the error is not a silent disconnect", on the
// reasoning that any other reply means the guest spoke. A DIAL FAILURE is not a
// silent disconnect either — and it is the opposite of proof of life. A guest
// that had already exited therefore produced a connection refused, was read as
// an answer, and the daemon logged "sandbox launched" over a dead VM: the exact
// bug MGIT-92 exists to remove, reintroduced by its own fix.
// Refs: MGIT-92
func TestLaunch_GuestUnreachable_FailsClosed(t *testing.T) {
	dialer := &unreachableDialer{}
	mgr, hv := launchManagerWithConsole(t, "mgit-guest: exited with error\n", dialer)

	info, err := mgr.Launch(context.Background(), launchOpts("MGIT-92e", model.NetworkModeNone))

	require.Error(t, err, "an unreachable guest is not a launched sandbox")
	assert.Nil(t, info)
	assert.ErrorIs(t, err, model.ErrGuestNotServing)
	assert.Contains(t, err.Error(), "mgit-guest: exited with error")
	require.Len(t, hv.vms, 1)
	assert.True(t, hv.vms[0].stopped)
	list, err := mgr.List(context.Background())
	require.NoError(t, err)
	assert.Empty(t, list)
}

// TestLaunch_GuestRefusesTheProbe_IsReady pins the other half of the same
// distinction: the probe names a program that does not exist, so the expected
// healthy answer IS an error — the guest reporting it could not start it. That
// reply proves the control plane serves and must let the launch stand.
// Refs: MGIT-92
func TestLaunch_GuestRefusesTheProbe_IsReady(t *testing.T) {
	dialer := &pipeDialer{}
	mgr := execManager(t, dialer)

	info, err := mgr.Launch(context.Background(), launchOpts("MGIT-92f", model.NetworkModeNone))

	require.NoError(t, err, "a guest that answers, even to refuse the probe, is serving")
	require.NotNil(t, info)
	assert.Positive(t, dialer.calls, "the probe really was sent")
}

// breakableDialer serves normally until goAway is called, after which every
// dial fails. It lets a test get past the launch — which now confirms the guest
// is serving (MGIT-92) — and then model the guest going away. Refs: MGIT-92
type breakableDialer struct {
	err   error
	inner GuestDialer
}

func (d *breakableDialer) goAway(err error) { d.err = err }

func (d *breakableDialer) DialGuest(ctx context.Context, id string) (net.Conn, error) {
	if d.err != nil {
		return nil, d.err
	}
	return d.inner.DialGuest(ctx, id)
}
