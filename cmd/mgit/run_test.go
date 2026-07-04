package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// staticwd returns a getwd func that always reports dir, for tests that
// must pin the working directory without touching the real process cwd.
func staticwd(dir string) func() (string, error) {
	return func() (string, error) { return dir, nil }
}

// runRun drives the `mgit run` command with an injected connector and
// cwd, capturing stdout+stderr.
func runRun(connect connectFunc, getwd func() (string, error), args ...string) (string, error) {
	cmd := newRunCmd(connect, getwd)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

// errConnect is a connector that always fails — models an unavailable daemon.
func errConnect(err error) connectFunc {
	return func(context.Context) (sandboxClient, error) { return nil, err }
}

// boundSandbox builds a List() result with one sandbox bound to wtPath.
func boundSandbox(wtPath, task string) []model.SandboxInfo {
	return []model.SandboxInfo{{ID: "01JSB", TaskID: task, WorktreePath: wtPath, State: model.StateRunning}}
}

// TestRun_RoutesCommandToBoundSandbox verifies a command is routed into
// the sandbox bound to the cwd's worktree and the guest cwd is the host
// cwd (identical-path mount). Refs: FR-17.11, MGIT-11.11.5
func TestRun_RoutesCommandToBoundSandbox(t *testing.T) {
	wt := filepath.FromSlash("/repo/wt")
	fc := &fakeSandboxClient{listResult: boundSandbox(wt, "MGIT-7.1"), execStdout: "ok\n"}

	out, err := runRun(okConnect(fc), staticwd(wt), "--", "npm", "test")

	require.NoError(t, err)
	assert.Equal(t, "MGIT-7.1", fc.execTask, "routed to the cwd-bound task")
	assert.Equal(t, []string{"npm", "test"}, fc.execReq.Command)
	assert.Equal(t, wt, fc.execReq.Dir, "guest cwd == host cwd (identical-path mount)")
	assert.Contains(t, out, "ok")
}

// TestRun_SubdirResolvesToWorktreeSandbox verifies a cwd nested inside a
// worktree still resolves to that worktree's sandbox (nearest-ancestor).
// Refs: MGIT-11.11.5
func TestRun_SubdirResolvesToWorktreeSandbox(t *testing.T) {
	wt := filepath.FromSlash("/repo/wt")
	sub := filepath.Join(wt, "pkg", "sub")
	fc := &fakeSandboxClient{listResult: boundSandbox(wt, "MGIT-7.1")}

	_, err := runRun(okConnect(fc), staticwd(sub), "--", "ls")

	require.NoError(t, err)
	assert.Equal(t, "MGIT-7.1", fc.execTask)
	assert.Equal(t, sub, fc.execReq.Dir, "guest cwd is the nested dir, not the worktree root")
}

// TestRun_NearestAncestorWins verifies the most specific worktree wins
// when sandboxes are bound to nested worktree paths. Refs: MGIT-11.11.5
func TestRun_NearestAncestorWins(t *testing.T) {
	outer := filepath.FromSlash("/repo/wt")
	inner := filepath.Join(outer, "inner")
	fc := &fakeSandboxClient{listResult: []model.SandboxInfo{
		{ID: "A", TaskID: "OUTER", WorktreePath: outer, State: model.StateRunning},
		{ID: "B", TaskID: "INNER", WorktreePath: inner, State: model.StateRunning},
	}}

	_, err := runRun(okConnect(fc), staticwd(filepath.Join(inner, "x")), "--", "ls")

	require.NoError(t, err)
	assert.Equal(t, "INNER", fc.execTask, "nearest-ancestor worktree wins")
}

// TestRun_PropagatesGuestExitCode verifies the guest exit status is
// propagated verbatim as an exitError. Refs: FR-17.11, MGIT-11.11.5
func TestRun_PropagatesGuestExitCode(t *testing.T) {
	wt := filepath.FromSlash("/repo/wt")
	fc := &fakeSandboxClient{listResult: boundSandbox(wt, "MGIT-7.1"), execCode: 7}

	_, err := runRun(okConnect(fc), staticwd(wt), "--", "false")

	var ee *exitError
	require.True(t, errors.As(err, &ee), "exit code surfaces as exitError")
	assert.Equal(t, 7, ee.code)
}

// TestRun_DaemonUnavailable_FailsClosedNoHostExec verifies that when the
// daemon cannot be reached the command errors and is NEVER run on the
// host. Refs: NFR-17.6, MGIT-11.11.5
func TestRun_DaemonUnavailable_FailsClosedNoHostExec(t *testing.T) {
	wt := filepath.FromSlash("/repo/wt")
	_, err := runRun(errConnect(errors.New("daemon down")), staticwd(wt), "--", "npm", "install")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "exit status", "fail-closed: no host exec, so no guest exit code")
}

// TestRun_NoSandboxBound_FailsClosed verifies that a cwd with no bound
// sandbox errors and never execs. Refs: NFR-17.6, MGIT-11.11.5
func TestRun_NoSandboxBound_FailsClosed(t *testing.T) {
	fc := &fakeSandboxClient{listResult: boundSandbox(filepath.FromSlash("/other/wt"), "OTHER")}

	_, err := runRun(okConnect(fc), staticwd(filepath.FromSlash("/repo/wt")), "--", "ls")

	require.Error(t, err)
	assert.Empty(t, fc.execTask, "Exec must not be called when no sandbox is bound (fail-closed)")
}

// TestRun_HostEnvNotForwarded_OnlyExplicitEnv verifies the host
// environment is never forwarded; only explicit --env injections pass.
// Refs: FR-17.3, MGIT-11.11.5
func TestRun_HostEnvNotForwarded_OnlyExplicitEnv(t *testing.T) {
	wt := filepath.FromSlash("/repo/wt")
	t.Setenv("SECRET_TOKEN", "should-not-leak")
	fc := &fakeSandboxClient{listResult: boundSandbox(wt, "MGIT-7.1")}

	_, err := runRun(okConnect(fc), staticwd(wt), "--env", "CI=1", "--", "make")

	require.NoError(t, err)
	assert.Equal(t, []string{"CI=1"}, fc.execReq.Env, "only explicit --env passes; host env never forwarded")
}

// TestRun_HealthQuery_ReportsAvailability verifies --check reports
// availability for the cwd without executing anything. Refs: MGIT-11.11.5
func TestRun_HealthQuery_ReportsAvailability(t *testing.T) {
	wt := filepath.FromSlash("/repo/wt")
	t.Run("available", func(t *testing.T) {
		fc := &fakeSandboxClient{listResult: boundSandbox(wt, "MGIT-7.1")}
		out, err := runRun(okConnect(fc), staticwd(wt), "--check")
		require.NoError(t, err)
		assert.Empty(t, fc.execTask, "--check never executes")
		assert.Contains(t, out, "available")
	})
	t.Run("unavailable_no_sandbox", func(t *testing.T) {
		fc := &fakeSandboxClient{listResult: nil}
		_, err := runRun(okConnect(fc), staticwd(wt), "--check")
		require.Error(t, err)
	})
	t.Run("unavailable_daemon_down", func(t *testing.T) {
		_, err := runRun(errConnect(errors.New("daemon down")), staticwd(wt), "--check")
		require.Error(t, err)
	})
}

// TestRun_RequiresCommand verifies that without --check a command is
// required. Refs: MGIT-11.11.5
func TestRun_RequiresCommand(t *testing.T) {
	wt := filepath.FromSlash("/repo/wt")
	_, err := runRun(okConnect(&fakeSandboxClient{listResult: boundSandbox(wt, "MGIT-7.1")}), staticwd(wt))
	assert.Error(t, err)
}

// TestRun_SuspendedSandboxRoutable verifies an idle-suspended sandbox is
// still a routing target (exec resumes it). Refs: NFR-17.3, MGIT-11.11.5
func TestRun_SuspendedSandboxRoutable(t *testing.T) {
	wt := filepath.FromSlash("/repo/wt")
	fc := &fakeSandboxClient{listResult: []model.SandboxInfo{
		{ID: "S", TaskID: "MGIT-7.1", WorktreePath: wt, State: model.StateSuspended},
	}}
	_, err := runRun(okConnect(fc), staticwd(wt), "--", "ls")
	require.NoError(t, err)
	assert.Equal(t, "MGIT-7.1", fc.execTask)
}

// TestRun_NonRoutableSandboxSkipped verifies a destroyed/landed sandbox
// for the cwd is not a routing target — fail-closed, no host exec.
// Refs: NFR-17.6, MGIT-11.11.5
func TestRun_NonRoutableSandboxSkipped(t *testing.T) {
	wt := filepath.FromSlash("/repo/wt")
	for _, state := range []string{model.StateDestroyed, model.StateLanded} {
		fc := &fakeSandboxClient{listResult: []model.SandboxInfo{
			{ID: "X", TaskID: "MGIT-7.1", WorktreePath: wt, State: state},
		}}
		_, err := runRun(okConnect(fc), staticwd(wt), "--", "ls")
		require.Error(t, err, "state %s must not be routable", state)
		assert.Empty(t, fc.execTask, "state %s: Exec must not be called", state)
	}
}

// TestRun_GuestExecError_Surfaced verifies a transport/exec error from
// the daemon is surfaced (not swallowed). Refs: MGIT-11.11.5
func TestRun_GuestExecError_Surfaced(t *testing.T) {
	wt := filepath.FromSlash("/repo/wt")
	fc := &fakeSandboxClient{listResult: boundSandbox(wt, "MGIT-7.1"), execErr: errors.New("vsock reset")}
	out, err := runRun(okConnect(fc), staticwd(wt), "--", "ls")
	require.Error(t, err)
	assert.Contains(t, out, "vsock reset")
}

// TestRun_GetwdError_FailsClosed verifies an unresolvable cwd is a hard
// error, never a host fallthrough. Refs: NFR-17.6, MGIT-11.11.5
func TestRun_GetwdError_FailsClosed(t *testing.T) {
	failwd := func() (string, error) { return "", errors.New("no cwd") }
	fc := &fakeSandboxClient{}
	_, err := runRun(okConnect(fc), failwd, "--", "ls")
	require.Error(t, err)
	assert.Empty(t, fc.execTask)
}

// TestRun_ListError_FailsClosed verifies a daemon List() error refuses
// the command rather than running it on the host. Refs: NFR-17.6, MGIT-11.11.5
func TestRun_ListError_FailsClosed(t *testing.T) {
	fc := &fakeSandboxClient{opErr: errors.New("daemon list failed")}
	_, err := runRun(okConnect(fc), staticwd(filepath.FromSlash("/repo/wt")), "--", "ls")
	require.Error(t, err)
	assert.Empty(t, fc.execTask)
}

// TestRunCmd_ProductionWiring verifies the production constructor builds a
// usable `run` command. Refs: MGIT-11.11.5
func TestRunCmd_ProductionWiring(t *testing.T) {
	cmd := runCmd()
	assert.Equal(t, "run", cmd.Name())
	assert.NotNil(t, cmd.RunE)
}

// TestRun_SymlinkedCwdAndRelativeRecord_StillMatches is the MGIT-57
// path-identity regression: the sandbox record and the runner's cwd must be
// compared in CANONICAL form. On macOS the temp dir is reached through the
// /var -> /private/var symlink, and `mgit work wt` used to record the
// worktree path exactly as the user typed it (relative "wt"), so
// `mgit run` inside the worktree matched nothing and failed closed.
// Refs: MGIT-57
func TestRun_SymlinkedCwdAndRelativeRecord_StillMatches(t *testing.T) {
	real := t.TempDir()
	wtReal := filepath.Join(real, "wt")
	require.NoError(t, os.MkdirAll(wtReal, 0o750))
	link := filepath.Join(t.TempDir(), "link")
	require.NoError(t, os.Symlink(real, link))

	// Record carries the canonical (symlink-resolved, absolute) path...
	canonical := canonicalPath(filepath.Join(link, "wt"))
	require.Equal(t, wtRealResolved(t, wtReal), canonical, "canonicalPath must resolve symlinks and absolutize")

	list := []model.SandboxInfo{{ID: "s1", TaskID: "T-1", State: model.StateRunning, WorktreePath: canonical}}

	// ...and the runner reaches the worktree THROUGH the symlink.
	cl := &fakeSandboxClient{listResult: list}
	_, dir, sb, err := resolveRun(context.Background(), okConnect(cl),
		func() (string, error) { return filepath.Join(link, "wt"), nil })
	require.NoError(t, err)
	assert.Equal(t, "s1", sb.ID)
	assert.Equal(t, canonical, dir, "resolveRun must canonicalize the cwd before matching")
}

// wtRealResolved resolves the expected canonical form of a t.TempDir path
// (t.TempDir itself may sit behind /var -> /private/var).
func wtRealResolved(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	require.NoError(t, err)
	return r
}
