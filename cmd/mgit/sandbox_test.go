package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/controlproto"
	"github.com/hyper-swe/mgit/internal/model"
)

// fakeSandboxClient is an in-memory sandboxClient for command tests.
type fakeSandboxClient struct {
	launched    *model.SandboxLaunchOptions
	execTask    string
	execReq     model.ExecRequest
	removedTID  string
	removeForce bool

	listResult []model.SandboxInfo
	statusInfo *model.SandboxInfo
	execStdout string
	execStderr string
	execCode   int
	execErr    error
	opErr      error

	landedTID  string
	landResult *controlproto.LandResult
}

func (f *fakeSandboxClient) Launch(_ context.Context, opts model.SandboxLaunchOptions) (*model.SandboxInfo, error) {
	if f.opErr != nil {
		return nil, f.opErr
	}
	f.launched = &opts
	return &model.SandboxInfo{ID: "01JSB", TaskID: opts.TaskID, State: model.StateCreated}, nil
}
func (f *fakeSandboxClient) Exec(_ context.Context, taskID string, req model.ExecRequest, stdout, stderr io.Writer) (int, error) {
	f.execTask, f.execReq = taskID, req
	if f.execErr != nil {
		return -1, f.execErr
	}
	_, _ = io.WriteString(stdout, f.execStdout)
	_, _ = io.WriteString(stderr, f.execStderr)
	return f.execCode, nil
}
func (f *fakeSandboxClient) List(context.Context) ([]model.SandboxInfo, error) {
	return f.listResult, f.opErr
}
func (f *fakeSandboxClient) Status(_ context.Context, taskID string) (*model.SandboxInfo, error) {
	if f.opErr != nil {
		return nil, f.opErr
	}
	if f.statusInfo != nil {
		return f.statusInfo, nil
	}
	return &model.SandboxInfo{ID: "01JSB", TaskID: taskID, State: model.StateRunning}, nil
}
func (f *fakeSandboxClient) Remove(_ context.Context, taskID string, force bool) error {
	f.removedTID, f.removeForce = taskID, force
	return f.opErr
}
func (f *fakeSandboxClient) Land(_ context.Context, taskID string) (*controlproto.LandResult, error) {
	f.landedTID = taskID
	if f.opErr != nil {
		return nil, f.opErr
	}
	if f.landResult != nil {
		return f.landResult, nil
	}
	return &controlproto.LandResult{Commits: 1, Branch: "task/" + taskID}, nil
}

// runSandbox executes the sandbox command tree with the given args and a
// connector, capturing stdout+stderr.
func runSandbox(connect connectFunc, args ...string) (string, error) {
	cmd := newSandboxCmd(connect)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func okConnect(c sandboxClient) connectFunc {
	return func(context.Context) (sandboxClient, error) { return c, nil }
}

// TestSandboxCmd_Help verifies the command group and every subcommand are
// registered and documented.
func TestSandboxCmd_Help(t *testing.T) {
	out, err := runSandbox(okConnect(&fakeSandboxClient{}), "--help")
	require.NoError(t, err)
	for _, sub := range []string{"launch", "exec", "land", "list", "remove", "status"} {
		assert.Contains(t, out, sub, "help lists the %s subcommand", sub)
	}
}

// TestSandboxLand_ReportsResult verifies the land command requires --task,
// routes the task to the client, and reports the landed commit count/branch.
func TestSandboxLand_ReportsResult(t *testing.T) {
	t.Run("lands", func(t *testing.T) {
		fc := &fakeSandboxClient{landResult: &controlproto.LandResult{Commits: 2, Branch: "task/MGIT-1"}}
		out, err := runSandbox(okConnect(fc), "land", "--task", "MGIT-1")
		require.NoError(t, err)
		assert.Equal(t, "MGIT-1", fc.landedTID)
		assert.Contains(t, out, "Landed 2 commit(s)")
		assert.Contains(t, out, "task/MGIT-1")
	})
	t.Run("nothing_new", func(t *testing.T) {
		fc := &fakeSandboxClient{landResult: &controlproto.LandResult{Commits: 0}}
		out, err := runSandbox(okConnect(fc), "land", "--task", "MGIT-1")
		require.NoError(t, err)
		assert.Contains(t, out, "Nothing to land")
	})
	t.Run("json", func(t *testing.T) {
		fc := &fakeSandboxClient{landResult: &controlproto.LandResult{Commits: 1, Branch: "task/MGIT-1"}}
		out, err := runSandbox(okConnect(fc), "land", "--task", "MGIT-1", "--json")
		require.NoError(t, err)
		assert.Contains(t, out, `"commits":1`)
	})
	t.Run("requires_task", func(t *testing.T) {
		_, err := runSandbox(okConnect(&fakeSandboxClient{}), "land")
		assert.Error(t, err)
	})
	t.Run("op_error", func(t *testing.T) {
		_, err := runSandbox(okConnect(&fakeSandboxClient{opErr: model.ErrSandboxNotFound}), "land", "--task", "MGIT-1")
		assert.Error(t, err)
	})
}

// TestSandboxExec_StreamsAndPropagatesExit verifies exec streams stdout/
// stderr from the (fake) daemon and propagates a non-zero exit code as an
// exitError carrying that status.
func TestSandboxExec_StreamsAndPropagatesExit(t *testing.T) {
	fc := &fakeSandboxClient{execStdout: "hello\n", execStderr: "warn\n", execCode: 3}
	out, err := runSandbox(okConnect(fc), "exec", "--task", "MGIT-1", "--", "echo", "hello")

	assert.Contains(t, out, "hello\n", "stdout is streamed")
	assert.Contains(t, out, "warn\n", "stderr is streamed")
	var ee *exitError
	require.ErrorAs(t, err, &ee, "a non-zero guest exit propagates as exitError")
	assert.Equal(t, 3, ee.code)
	assert.Equal(t, "MGIT-1", fc.execTask)
	assert.Equal(t, []string{"echo", "hello"}, fc.execReq.Command, "argv passed as a list (no shell)")
}

// TestSandboxExec_ZeroExit_NoError verifies a clean exit returns no error.
func TestSandboxExec_ZeroExit_NoError(t *testing.T) {
	_, err := runSandbox(okConnect(&fakeSandboxClient{execCode: 0}), "exec", "--task", "MGIT-1", "--", "true")
	require.NoError(t, err)
}

// TestSandboxExec_HostEnvNotForwarded verifies only explicit --env entries
// are sent; the host environment is never forwarded into the guest.
func TestSandboxExec_HostEnvNotForwarded(t *testing.T) {
	t.Setenv("MGIT_SECRET", "do-not-leak")
	fc := &fakeSandboxClient{}
	_, err := runSandbox(okConnect(fc), "exec", "--task", "MGIT-1", "--env", "FOO=bar", "--", "env")
	require.NoError(t, err)
	assert.Equal(t, []string{"FOO=bar"}, fc.execReq.Env, "only explicit --env is sent")
	for _, e := range fc.execReq.Env {
		assert.NotContains(t, e, "do-not-leak", "host env must not leak into the guest")
	}
}

// TestSandboxCmd_BackendUnavailable_FailsClearly verifies that when the
// daemon/backend cannot be reached, exec fails clearly and runs nothing —
// no silent local fallback.
func TestSandboxCmd_BackendUnavailable_FailsClearly(t *testing.T) {
	fc := &fakeSandboxClient{}
	failConnect := func(context.Context) (sandboxClient, error) {
		return nil, assert.AnError
	}
	out, err := runSandbox(failConnect, "exec", "--task", "MGIT-1", "--", "rm", "-rf", "/")
	require.Error(t, err)
	assert.Contains(t, out, "mgit sandbox:", "the failure is reported clearly")
	assert.Empty(t, fc.execTask, "nothing was executed when the backend was unavailable")
	var ee *exitError
	assert.False(t, errors.As(err, &ee), "an unavailable backend is a plain failure, not a guest exit code")
}

// TestSandboxCmd_FlagWiring verifies each subcommand's flags wire through
// to the client call.
func TestSandboxCmd_FlagWiring(t *testing.T) {
	t.Run("launch", func(t *testing.T) {
		fc := &fakeSandboxClient{}
		_, err := runSandbox(okConnect(fc), "launch",
			"--task", "MGIT-2", "--worktree", "/w", "--image", "img@sha256:"+strings.Repeat("a", 64),
			"--network", "allowlist", "--allow", "example.com")
		require.NoError(t, err)
		require.NotNil(t, fc.launched)
		assert.Equal(t, "MGIT-2", fc.launched.TaskID)
		assert.Equal(t, "/w", fc.launched.WorktreePath)
		assert.Equal(t, "allowlist", fc.launched.Network.Mode)
		assert.Equal(t, []string{"example.com"}, fc.launched.Network.Allowlist)
	})
	t.Run("launch_missing_required", func(t *testing.T) {
		_, err := runSandbox(okConnect(&fakeSandboxClient{}), "launch", "--task", "MGIT-2")
		assert.Error(t, err, "missing --worktree/--image is rejected")
	})
	t.Run("remove_force", func(t *testing.T) {
		fc := &fakeSandboxClient{}
		_, err := runSandbox(okConnect(fc), "remove", "MGIT-3", "--force")
		require.NoError(t, err)
		assert.Equal(t, "MGIT-3", fc.removedTID)
		assert.True(t, fc.removeForce)
	})
	t.Run("exec_missing_task", func(t *testing.T) {
		_, err := runSandbox(okConnect(&fakeSandboxClient{}), "exec", "--", "ls")
		assert.Error(t, err, "exec without --task is rejected")
	})
}

// TestSandboxCmd_JSONOutput verifies --json structured output for list and
// status.
func TestSandboxCmd_JSONOutput(t *testing.T) {
	t.Run("list", func(t *testing.T) {
		fc := &fakeSandboxClient{listResult: []model.SandboxInfo{{ID: "01JSB", TaskID: "MGIT-1", State: model.StateRunning}}}
		out, err := runSandbox(okConnect(fc), "list", "--json")
		require.NoError(t, err)
		var got []model.SandboxInfo
		require.NoError(t, json.Unmarshal([]byte(out), &got))
		require.Len(t, got, 1)
		assert.Equal(t, "MGIT-1", got[0].TaskID)
	})
	t.Run("status", func(t *testing.T) {
		out, err := runSandbox(okConnect(&fakeSandboxClient{}), "status", "MGIT-9", "--json")
		require.NoError(t, err)
		var got model.SandboxInfo
		require.NoError(t, json.Unmarshal([]byte(out), &got))
		assert.Equal(t, "MGIT-9", got.TaskID)
	})
	t.Run("list_empty_human", func(t *testing.T) {
		out, err := runSandbox(okConnect(&fakeSandboxClient{}), "list")
		require.NoError(t, err)
		assert.Contains(t, out, "no sandboxes")
	})
}

// TestSandboxCmd_OpError_Surfaces verifies a daemon-side error surfaces on
// the non-streaming verbs.
func TestSandboxCmd_OpError_Surfaces(t *testing.T) {
	for _, args := range [][]string{{"status", "MGIT-x"}, {"list"}, {"remove", "MGIT-x"}} {
		_, err := runSandbox(okConnect(&fakeSandboxClient{opErr: model.ErrSandboxNotFound}), args...)
		assert.Error(t, err, "op error surfaces for %v", args)
	}
}

// TestSandboxCmd_ProductionTreeBuilds verifies the production command group
// (wired to the real connector) builds and documents its help without
// touching a daemon.
func TestSandboxCmd_ProductionTreeBuilds(t *testing.T) {
	cmd := sandboxCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--help"})
	require.NoError(t, cmd.Execute())
	assert.Contains(t, out.String(), "microVM sandbox")
}

// TestExitError_Message verifies the exit-status error renders its code.
func TestExitError_Message(t *testing.T) {
	assert.Contains(t, (&exitError{code: 7}).Error(), "7")
}

// TestRuntimeBase verifies XDG_RUNTIME_DIR is preferred, with a temp-dir
// fallback.
func TestRuntimeBase(t *testing.T) {
	t.Run("xdg_set", func(t *testing.T) {
		t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
		assert.Equal(t, "/run/user/1000", runtimeBase())
	})
	t.Run("xdg_unset", func(t *testing.T) {
		t.Setenv("XDG_RUNTIME_DIR", "")
		assert.Equal(t, os.TempDir(), runtimeBase())
	})
}

// TestLocateSandboxd verifies the lookup returns either a path or a clear
// not-found error (never panics), independent of install layout.
func TestLocateSandboxd(t *testing.T) {
	path, err := locateSandboxd()
	if err != nil {
		assert.Contains(t, err.Error(), "not found")
	} else {
		assert.NotEmpty(t, path)
	}
}

// TestResolveSandboxPaths verifies path derivation: durable host config
// under .mgit, a short owner-only runtime dir for the socket, and a clear
// error outside a repository.
func TestResolveSandboxPaths(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", xdg)
	repo := t.TempDir()
	t.Run("not_a_repo", func(t *testing.T) {
		_, err := resolveSandboxPaths(repo)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not an mgit repository")
	})
	t.Run("repo", func(t *testing.T) {
		require.NoError(t, mkMgitDir(repo))
		p, err := resolveSandboxPaths(repo)
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(repo, ".mgit", "sandbox"), p.hostRoot)
		assert.True(t, strings.HasSuffix(p.socket, "d.sock"))
		// The suffix mgit derives below the runtime base must stay short, so
		// the socket fits the unix sun_path limit (~104) on a short base
		// like /run/user/<uid>. Independent of this test's long XDG base.
		assert.Less(t, len(p.socket)-len(xdg), 40, "derived runtime suffix must stay short for sun_path")
	})
}

func mkMgitDir(repo string) error {
	return os.MkdirAll(filepath.Join(repo, ".mgit"), 0o700)
}
