package microvm

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"runtime"
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
