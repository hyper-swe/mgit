package main

import (
	"context"
	"encoding/json"
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
	bin := buildMgitHookTestBinary(t)
	repo := t.TempDir()
	require.NoError(t, runMgitHookTest(t, bin, repo, "init"))

	t.Run("bash_tool_no_sandbox_asks", func(t *testing.T) {
		out, exitErr := runMgitHookTestStdin(t, bin, repo,
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
		out, exitErr := runMgitHookTestStdin(t, bin, repo,
			`{"hook_event_name":"PreToolUse","tool_name":"Read","cwd":"`+repo+`"}`,
			"sandbox", "claude-hook")
		assert.NoError(t, exitErr)
		assert.JSONEq(t, "{}", out, "a non-Bash tool call must pass through untouched")
	})
}

func buildMgitHookTestBinary(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok, "locate this test file to find the module root")
	projectRoot := filepath.Join(filepath.Dir(thisFile), "..", "..")

	bin := filepath.Join(t.TempDir(), "mgit-hooktest")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-o", bin, "./cmd/mgit/") //nolint:gosec // fixed argv, test-only
	cmd.Dir = projectRoot
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "build mgit: %s", string(out))
	return bin
}

func runMgitHookTest(t *testing.T, bin, dir string, args ...string) error {
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

func runMgitHookTestStdin(t *testing.T, bin, dir, stdin string, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...) //nolint:gosec // fixed argv, test-only
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.Output()
	return string(out), err
}
