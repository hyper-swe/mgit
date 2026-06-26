package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// fakeWorktreeAdder records the add it was asked to perform and materializes
// the worktree dir so adapter writes have a real path to target. It models
// WorktreeService.Add without a full repo, keeping the orchestration tests
// host-side and KVM-free. Refs: MGIT-34, FR-16
type fakeWorktreeAdder struct {
	gotOpts model.WorktreeAddOptions
	err     error
}

func (f *fakeWorktreeAdder) add(_ context.Context, opts model.WorktreeAddOptions) (*model.WorktreeInfo, error) {
	f.gotOpts = opts
	if f.err != nil {
		return nil, f.err
	}
	if err := os.MkdirAll(opts.Path, 0o700); err != nil {
		return nil, err
	}
	branch := opts.Branch
	if branch == "" {
		branch = "task/" + opts.TaskID
	}
	return &model.WorktreeInfo{
		Path: opts.Path, Name: filepath.Base(opts.Path), Branch: branch,
		TaskID: opts.TaskID, AgentID: opts.AgentID, CreatedAt: time.Unix(0, 0).UTC(),
	}, nil
}

// runWorkSetup drives workSetup with an injected adder + connector and a
// fresh deps struct, capturing the human/warn output.
func runWorkSetup(t *testing.T, adder *fakeWorktreeAdder, opts workOptions, connect connectFunc) (string, *model.WorktreeInfo, error) {
	t.Helper()
	var out bytes.Buffer
	deps := workDeps{
		addWorktree:    adder.add,
		writeAdapters:  injectAgentAdapters,
		upsertEnvDoc:   upsertWorktreeEnvDoc,
		connect:        connect,
		mgitBinForDocs: "mgit",
	}
	wt, err := workSetup(context.Background(), &out, deps, opts)
	return out.String(), wt, err
}

// TestWorkSetup_CreatesAndBindsWorktree verifies the flow provisions the
// mgit worktree bound to the task (reusing the worktree-add path), the
// single hard-required leg. Refs: MGIT-34, FR-16
func TestWorkSetup_CreatesAndBindsWorktree(t *testing.T) {
	adder := &fakeWorktreeAdder{}
	path := filepath.Join(t.TempDir(), "wt")
	_, wt, err := runWorkSetup(t, adder, workOptions{Path: path, TaskID: "MGIT-7.1"}, nil)

	require.NoError(t, err)
	require.NotNil(t, wt)
	assert.Equal(t, "MGIT-7.1", adder.gotOpts.TaskID, "worktree bound to the task")
	assert.Equal(t, path, adder.gotOpts.Path)
	assert.Equal(t, "task/MGIT-7.1", wt.Branch)
}

// TestWorkSetup_WritesAdapterWiring verifies the agent-integration files are
// written into the worktree: the Claude settings hook routing through `mgit
// run`, the CLAUDE.md sandbox-env block, and the cooperative shims.
// Refs: MGIT-34, MGIT-11.11.1, MGIT-11.11.2, MGIT-11.11.3
func TestWorkSetup_WritesAdapterWiring(t *testing.T) {
	adder := &fakeWorktreeAdder{}
	path := filepath.Join(t.TempDir(), "wt")
	_, _, err := runWorkSetup(t, adder, workOptions{Path: path, TaskID: "MGIT-7.2"}, nil)
	require.NoError(t, err)

	// .claude/settings.json routes the agent's Bash through the mgit hook.
	settings, rerr := os.ReadFile(filepath.Join(path, ".claude", "settings.json")) //nolint:gosec // test temp path
	require.NoError(t, rerr)
	var doc map[string]any
	require.NoError(t, json.Unmarshal(settings, &doc))
	assert.Contains(t, string(settings), claudeHookCommand, "settings routes through the mgit sandbox hook")
	assert.Contains(t, string(settings), "Bash(mgit run:*)", "the routed command is pre-approved")

	// CLAUDE.md carries the sandbox-env knowledge block.
	md, rerr := os.ReadFile(filepath.Join(path, "CLAUDE.md")) //nolint:gosec // test temp path
	require.NoError(t, rerr)
	assert.Contains(t, string(md), "microVM", "CLAUDE.md describes the sandbox environment")
	assert.Contains(t, string(md), path, "CLAUDE.md cites the identical-path mount")

	// Cooperative generic adapter is installed (PATH shim via .envrc).
	_, rerr = os.Stat(filepath.Join(path, ".envrc"))
	assert.NoError(t, rerr, "cooperative PATH-shim adapter installed")
}

// TestWorkSetup_WorktreeFailure_Aborts verifies a worktree-add failure is a
// hard error: nothing is wired when the worktree cannot be created.
// Refs: MGIT-34, FR-16
func TestWorkSetup_WorktreeFailure_Aborts(t *testing.T) {
	adder := &fakeWorktreeAdder{err: errors.New("task already bound")}
	path := filepath.Join(t.TempDir(), "wt")
	_, wt, err := runWorkSetup(t, adder, workOptions{Path: path, TaskID: "MGIT-7.3"}, nil)

	require.Error(t, err)
	assert.Nil(t, wt)
	_, statErr := os.Stat(filepath.Join(path, ".claude", "settings.json"))
	assert.True(t, os.IsNotExist(statErr), "no adapters written when worktree creation fails")
}

// TestWorkSetup_Idempotent_ReRun verifies a second run over the same path
// does not duplicate or garble the wiring (the merge/upsert helpers are
// idempotent). Refs: MGIT-34, MGIT-11.11.1, MGIT-11.11.2
func TestWorkSetup_Idempotent_ReRun(t *testing.T) {
	adder := &fakeWorktreeAdder{}
	path := filepath.Join(t.TempDir(), "wt")
	opts := workOptions{Path: path, TaskID: "MGIT-7.4"}

	_, _, err := runWorkSetup(t, adder, opts, nil)
	require.NoError(t, err)
	first, rerr := os.ReadFile(filepath.Join(path, ".claude", "settings.json")) //nolint:gosec // test temp path
	require.NoError(t, rerr)
	firstMd, rerr := os.ReadFile(filepath.Join(path, "CLAUDE.md")) //nolint:gosec // test temp path
	require.NoError(t, rerr)

	_, _, err = runWorkSetup(t, adder, opts, nil)
	require.NoError(t, err)
	second, rerr := os.ReadFile(filepath.Join(path, ".claude", "settings.json")) //nolint:gosec // test temp path
	require.NoError(t, rerr)
	secondMd, rerr := os.ReadFile(filepath.Join(path, "CLAUDE.md")) //nolint:gosec // test temp path
	require.NoError(t, rerr)

	assert.Equal(t, string(first), string(second), "re-run must not duplicate settings hooks")
	assert.Equal(t, string(firstMd), string(secondMd), "re-run must not duplicate the CLAUDE.md block")
}

// TestWorkSetup_SandboxRequested_Launches verifies that with --sandbox the
// bound sandbox is launched and the CLAUDE.md env block is regenerated to the
// live posture. Refs: MGIT-34, MGIT-11.11.2
func TestWorkSetup_SandboxRequested_Launches(t *testing.T) {
	adder := &fakeWorktreeAdder{}
	path := filepath.Join(t.TempDir(), "wt")
	fc := &fakeSandboxClient{}
	opts := workOptions{
		Path: path, TaskID: "MGIT-7.5", LaunchSandbox: true,
		Image: validationStubImageRef, Network: model.NetworkModeAllowlist, Allow: []string{"github.com"},
	}
	out, wt, err := runWorkSetup(t, adder, opts, okConnect(fc))

	require.NoError(t, err)
	require.NotNil(t, wt)
	require.NotNil(t, fc.launched, "sandbox launch was requested")
	assert.Equal(t, "MGIT-7.5", fc.launched.TaskID)
	assert.Equal(t, path, fc.launched.WorktreePath, "sandbox mounts the worktree")
	assert.Equal(t, model.NetworkModeAllowlist, fc.launched.Network.Mode)
	assert.Contains(t, out, "sandbox", "launch outcome reported")
}

// TestWorkSetup_SandboxUnavailable_DegradesGracefully verifies that when the
// sandbox backend is unavailable, the worktree + adapter wiring still
// succeed; only the sandbox-routing leg fails (fail-closed) and is reported
// cleanly. Refs: MGIT-34, NFR-17.6
func TestWorkSetup_SandboxUnavailable_DegradesGracefully(t *testing.T) {
	adder := &fakeWorktreeAdder{}
	path := filepath.Join(t.TempDir(), "wt")
	opts := workOptions{
		Path: path, TaskID: "MGIT-7.6", LaunchSandbox: true,
		Image: validationStubImageRef, Network: model.NetworkModeNone,
	}
	out, wt, err := runWorkSetup(t, adder, opts, errConnect(errors.New("daemon unavailable")))

	// The flow as a whole succeeds: the worktree exists and is wired.
	require.NoError(t, err, "sandbox unavailability must not fail the worktree+adapter flow")
	require.NotNil(t, wt)
	_, statErr := os.Stat(filepath.Join(path, ".claude", "settings.json"))
	require.NoError(t, statErr, "adapters still written despite no sandbox")
	_, mdErr := os.Stat(filepath.Join(path, "CLAUDE.md"))
	require.NoError(t, mdErr, "CLAUDE.md still written despite no sandbox")

	// The sandbox leg is reported as degraded, not silently dropped.
	assert.Contains(t, out, "sandbox", "the failed sandbox leg is reported")
	assert.Contains(t, out, "mgit sandbox launch", "a clear remedy is given")
}

// TestWorkSetup_NoSandboxFlag_SkipsLaunch verifies the default (no --sandbox)
// never contacts the daemon: a connector that would fail is never called.
// Refs: MGIT-34
func TestWorkSetup_NoSandboxFlag_SkipsLaunch(t *testing.T) {
	adder := &fakeWorktreeAdder{}
	path := filepath.Join(t.TempDir(), "wt")
	called := false
	connect := func(context.Context) (sandboxClient, error) {
		called = true
		return nil, errors.New("should not be called")
	}
	_, _, err := runWorkSetup(t, adder, workOptions{Path: path, TaskID: "MGIT-7.7"}, connect)

	require.NoError(t, err)
	assert.False(t, called, "no --sandbox means the daemon is never contacted")
}

// TestWorkCmd_RequiresTask verifies the CLI rejects a missing --task.
// Refs: MGIT-34
func TestWorkCmd_RequiresTask(t *testing.T) {
	cmd := newWorkCmd(func(context.Context, *App, workOptions) error { return nil })
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{filepath.Join(t.TempDir(), "wt")})
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--task")
}
