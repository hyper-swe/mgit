// Package container tests verify the reduced-isolation fallback
// backend per MGIT-11.5.4. The podman CLI is faked behind the runner
// seam; argument construction carries the isolation contract.
// Refs: FR-17.15, FR-17.3, FR-17.14
package container

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// fakeRunner records podman invocations and scripts their results.
type fakeRunner struct {
	mu    sync.Mutex
	calls [][]string
	// results maps the first argument (subcommand) to scripted output.
	results map[string]struct {
		out []byte
		err error
	}
}

func (r *fakeRunner) run(_ context.Context, args ...string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, args)
	if res, ok := r.results[args[0]]; ok {
		return res.out, res.err
	}
	return nil, nil
}

func (r *fakeRunner) callsFor(sub string) [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out [][]string
	for _, call := range r.calls {
		if call[0] == sub {
			out = append(out, call)
		}
	}
	return out
}

func hasArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func testManager(t *testing.T, runner *fakeRunner, sensitive ...string) *Manager {
	t.Helper()
	if runner.results == nil {
		runner.results = map[string]struct {
			out []byte
			err error
		}{}
	}
	mgr, err := NewManager(Config{
		Runner:         runner,
		SensitivePaths: sensitive,
		Logger:         slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		Clock:          func() time.Time { return time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC) },
	})
	require.NoError(t, err)
	return mgr
}

func containerOpts(t *testing.T, mode string) model.SandboxLaunchOptions {
	t.Helper()
	worktree := t.TempDir()
	return model.SandboxLaunchOptions{
		TaskID:       "MGIT-4.2",
		WorktreePath: worktree,
		ImageRef:     "go-node@sha256:" + strings.Repeat("a", 64),
		Network:      model.NetworkPolicy{Mode: mode},
		CPUs:         2,
		MemoryMB:     1024,
		TTL:          time.Hour,
	}
}

// TestContainer_Launch_IsolationContract verifies the run invocation
// carries the contract: worktree at the identical path, sensitive
// paths read-only (FR-17.14), resource caps, and the network mapping.
// Refs: FR-17.3, FR-17.7, FR-17.14
func TestContainer_Launch_IsolationContract(t *testing.T) {
	runner := &fakeRunner{}
	mgr := testManager(t, runner, ".git/hooks", ".envrc")
	ctx := context.Background()

	opts := containerOpts(t, model.NetworkModeNone)
	require.NoError(t, os.MkdirAll(filepath.Join(opts.WorktreePath, ".git", "hooks"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(opts.WorktreePath, ".envrc"), nil, 0o600))

	info, err := mgr.Launch(ctx, opts)
	require.NoError(t, err)
	assert.Equal(t, model.BackendContainer, info.Backend)
	assert.Equal(t, model.StateRunning, info.State)

	runs := runner.callsFor("run")
	require.Len(t, runs, 1)
	args := runs[0]

	assert.True(t, hasArg(args, "--network"), "network flag present")
	assert.True(t, hasArg(args, "none"), "mode none maps to --network none (FR-17.7)")
	assert.True(t, hasArg(args, opts.WorktreePath+":"+opts.WorktreePath),
		"worktree mounted rw at the identical path (FR-17.3)")
	assert.True(t, hasArg(args, filepath.Join(opts.WorktreePath, ".git", "hooks")+":"+filepath.Join(opts.WorktreePath, ".git", "hooks")+":ro"),
		"sensitive dirs remounted read-only (FR-17.14)")
	assert.True(t, hasArg(args, filepath.Join(opts.WorktreePath, ".envrc")+":"+filepath.Join(opts.WorktreePath, ".envrc")+":ro"),
		"sensitive files remounted read-only (FR-17.14)")
	assert.True(t, hasArg(args, "--memory"), "memory cap applied")
	assert.True(t, hasArg(args, "1024m"))
	assert.True(t, hasArg(args, "--cpus"), "cpu cap applied")

	for _, arg := range args {
		assert.False(t, strings.Contains(arg, "--privileged"), "never privileged")
	}

	t.Run("registered_before_return", func(t *testing.T) {
		listed, err := mgr.List(ctx)
		require.NoError(t, err)
		require.Len(t, listed, 1)
		assert.Equal(t, info.ID, listed[0].ID)
	})

	t.Run("allowlist_mode_refused_until_proxy", func(t *testing.T) {
		_, err := mgr.Launch(ctx, containerOpts(t, model.NetworkModeAllowlist))
		assert.ErrorIs(t, err, model.ErrNetworkPolicyViolation,
			"allowlist enforcement needs the egress proxy (MGIT-11.7.2); mapping it to open would be a silent SEC-04 violation")
	})

	t.Run("open_mode_uses_user_network", func(t *testing.T) {
		_, err := mgr.Launch(ctx, containerOpts(t, model.NetworkModeOpen))
		require.NoError(t, err)
		runs := runner.callsFor("run")
		assert.True(t, hasArg(runs[len(runs)-1], "slirp4netns"), "open mode uses the rootless user network")
	})
}

// TestContainer_ExecStopRemove verifies the lifecycle verbs delegate
// to the container runtime and teardown deregisters. Refs: FR-17.19
func TestContainer_ExecStopRemove(t *testing.T) {
	runner := &fakeRunner{results: map[string]struct {
		out []byte
		err error
	}{
		"exec": {out: []byte("hello\n")},
	}}
	mgr := testManager(t, runner)
	ctx := context.Background()

	info, err := mgr.Launch(ctx, containerOpts(t, model.NetworkModeNone))
	require.NoError(t, err)

	res, err := mgr.Exec(ctx, info.ID, model.ExecRequest{Command: []string{"echo", "hello"}})
	require.NoError(t, err)
	assert.Equal(t, "hello\n", string(res.Stdout))
	assert.Zero(t, res.ExitCode)
	execCalls := runner.callsFor("exec")
	require.Len(t, execCalls, 1)
	assert.True(t, hasArg(execCalls[0], "echo"))

	require.NoError(t, mgr.Stop(ctx, info.ID, false))
	resolved, err := mgr.Resolve(ctx, info.ID)
	require.NoError(t, err)
	assert.Equal(t, model.StateSuspended, resolved.State)

	require.NoError(t, mgr.Remove(ctx, info.ID, true))
	rmCalls := runner.callsFor("rm")
	require.Len(t, rmCalls, 1)
	assert.True(t, hasArg(rmCalls[0], "-f"), "forced removal")

	listed, err := mgr.List(ctx)
	require.NoError(t, err)
	assert.Empty(t, listed)

	t.Run("invalid_exec_request_rejected", func(t *testing.T) {
		info2, err := mgr.Launch(ctx, containerOpts(t, model.NetworkModeNone))
		require.NoError(t, err)
		_, err = mgr.Exec(ctx, info2.ID, model.ExecRequest{})
		assert.Error(t, err, "empty command rejected by validation")
	})

	t.Run("unknown_ids_not_found", func(t *testing.T) {
		_, err := mgr.Exec(ctx, "01JXNOPE", model.ExecRequest{Command: []string{"true"}})
		assert.ErrorIs(t, err, model.ErrSandboxNotFound)
		assert.ErrorIs(t, mgr.Stop(ctx, "01JXNOPE", false), model.ErrSandboxNotFound)
		assert.ErrorIs(t, mgr.Remove(ctx, "01JXNOPE", false), model.ErrSandboxNotFound)
		_, err = mgr.Resolve(ctx, "01JXNOPE")
		assert.ErrorIs(t, err, model.ErrSandboxNotFound)
	})
}

// TestContainer_Guards covers constructor and launch failure paths.
func TestContainer_Guards(t *testing.T) {
	ctx := context.Background()

	t.Run("constructor_guards", func(t *testing.T) {
		_, err := NewManager(Config{})
		assert.Error(t, err, "nil runner rejected")
		_, err = NewManager(Config{Runner: &fakeRunner{}, Clock: time.Now})
		assert.Error(t, err, "nil logger rejected")
		_, err = NewManager(Config{Runner: &fakeRunner{}, Logger: slog.Default()})
		assert.Error(t, err, "nil clock rejected")
	})

	t.Run("invalid_options_rejected", func(t *testing.T) {
		mgr := testManager(t, &fakeRunner{})
		opts := containerOpts(t, model.NetworkModeNone)
		opts.TaskID = ""
		_, err := mgr.Launch(ctx, opts)
		assert.Error(t, err)
	})

	t.Run("runtime_failure_surfaces", func(t *testing.T) {
		runner := &fakeRunner{results: map[string]struct {
			out []byte
			err error
		}{
			"run": {err: assert.AnError},
		}}
		mgr := testManager(t, runner)
		_, err := mgr.Launch(ctx, containerOpts(t, model.NetworkModeNone))
		require.Error(t, err)
		listed, err := mgr.List(ctx)
		require.NoError(t, err)
		assert.Empty(t, listed, "a failed launch is not registered")
	})
}
