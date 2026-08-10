package main

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/agentadapter"
)

// TestClaudeHook_RealProcess_StdinToStdoutContract is the real-process proof
// claude_hook_test.go lacked: every existing case drives sandboxClaudeHookCmd
// in-process with an injected fake connector, never the compiled binary
// reading real stdin through the production sandbox-availability check. This
// spawns the actual `mgit sandbox claude-hook` binary exactly as a Claude
// Code harness would -- JSON on stdin, a JSON decision on stdout, exit 0
// regardless of outcome (the hook contract, claude_hook.go). No daemon is
// running in this environment, so the real production connect path
// genuinely reports the sandbox unavailable; that IS a real, common harness
// state (no `--sandbox` requested) and is exactly the "ask" fallback this
// hook exists to make honest rather than silently permissive. Refs: MGIT-11.11.1
func TestClaudeHook_RealProcess_StdinToStdoutContract(t *testing.T) {
	bin := buildMgitTestBinary(t)
	repo := t.TempDir()
	require.NoError(t, runMgitTest(t, bin, repo, "init"))

	t.Run("bash_tool_no_sandbox_asks", func(t *testing.T) {
		out, exitErr := runMgitTestStdin(t, bin, repo,
			`{"hook_event_name":"PreToolUse","tool_name":"Bash","cwd":"`+repo+`","tool_input":{"command":"echo hi"}}`,
			"sandbox", "claude-hook")
		assert.NoError(t, exitErr, "the hook contract exits 0 even when the decision is 'ask'")

		var decoded agentadapter.HookOutput
		require.NoError(t, json.Unmarshal([]byte(out), &decoded), "stdout must be the decision JSON, nothing else: %s", out)
		assert.Equal(t, "PreToolUse", decoded.HookSpecificOutput.HookEventName)
		assert.Equal(t, agentadapter.DecisionAsk, decoded.HookSpecificOutput.PermissionDecision)
		assert.Contains(t, decoded.HookSpecificOutput.PermissionDecisionReason, "unavailable")
	})

	t.Run("non_bash_tool_untouched", func(t *testing.T) {
		out, exitErr := runMgitTestStdin(t, bin, repo,
			`{"hook_event_name":"PreToolUse","tool_name":"Read","cwd":"`+repo+`"}`,
			"sandbox", "claude-hook")
		assert.NoError(t, exitErr)
		assert.JSONEq(t, "{}", out, "a non-Bash tool call must pass through untouched")
	})
}

// buildMgitTestBinary compiles the real mgit binary into a temp dir. Tests use
// it when the thing under test is a process-level contract (exit status,
// stdin/stdout wiring) that an in-process cobra invocation cannot observe.
func buildMgitTestBinary(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok, "locate this test file to find the module root")
	projectRoot := filepath.Join(filepath.Dir(thisFile), "..", "..")

	bin := filepath.Join(t.TempDir(), "mgit-hooktest")
	// 60s was too tight for a cold module/build cache -- this test's own `go
	// build` subprocess got killed mid-download on this branch's first real
	// CI run (a fresh runner with nothing cached yet). 180s covers a cold
	// cache; a warm one (the common case) still finishes in a few seconds.
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-o", bin, "./cmd/mgit/") //nolint:gosec // fixed argv, test-only
	cmd.Dir = projectRoot
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "build mgit: %s", string(out))
	return bin
}

func runMgitTest(t *testing.T, bin, dir string, args ...string) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...) //nolint:gosec // fixed argv, test-only
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("mgit %v: %v\n%s", args, err, out)
	}
	return err
}

// runMgitTestExit executes one mgit command and returns its combined output
// together with the process exit status — the value an agent's shell sees and
// branches on. A command that could not be started at all yields -1.
func runMgitTestExit(t *testing.T, bin, dir string, args ...string) (string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...) //nolint:gosec // fixed argv, test-only
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else {
			code = -1
		}
	}
	t.Logf("mgit %v -> exit %d\n%s", args, code, out)
	return string(out), code
}

func runMgitTestStdin(t *testing.T, bin, dir, stdin string, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...) //nolint:gosec // fixed argv, test-only
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.Output()
	return string(out), err
}
