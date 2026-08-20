package main

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/agentadapter"
)

// Codex and Cursor are not installed in this environment and cannot be
// installed in CI, so the vendor contracts these hooks satisfy are verified
// from vendor documentation, not by running the vendors' tools. What CAN be
// proven here — and is — is mgit's own half of each contract, driven as a
// REAL PROCESS exactly as each harness drives it: JSON on stdin, one JSON
// document on stdout, exit 0 whatever the decision.
//
// No daemon runs in this environment, so the production availability check
// genuinely reports the sandbox unavailable. That is the case that matters
// most: it is the state in which a fail-open hook would hand an unexamined
// command to the host. Refs: MGIT-149
func TestCodexHook_RealProcess_UnavailableSandboxIsDeniedNotAsked(t *testing.T) {
	bin := buildMgitTestBinary(t)
	repo := t.TempDir()
	require.NoError(t, runMgitTest(t, bin, repo, "init"))

	t.Run("bash_without_sandbox_is_denied", func(t *testing.T) {
		out, exitErr := runMgitTestStdin(t, bin, repo,
			`{"hook_event_name":"PreToolUse","tool_name":"Bash","cwd":"`+repo+`","tool_input":{"command":"echo hi"}}`,
			"sandbox", "codex-hook")
		assert.NoError(t, exitErr, "the hook contract exits 0 even when it refuses")

		var decoded agentadapter.HookOutput
		require.NoError(t, json.Unmarshal([]byte(out), &decoded), "stdout must be the decision JSON alone: %s", out)
		assert.Equal(t, agentadapter.DecisionDeny, decoded.HookSpecificOutput.PermissionDecision,
			"Codex ignores 'ask'; anything but deny here runs on the host")
		assert.NotEqual(t, agentadapter.DecisionAsk, decoded.HookSpecificOutput.PermissionDecision)
		assert.Nil(t, decoded.HookSpecificOutput.UpdatedInput)
	})

	t.Run("malformed_payload_is_denied_not_permitted", func(t *testing.T) {
		out, exitErr := runMgitTestStdin(t, bin, repo, `{not json`, "sandbox", "codex-hook")
		assert.NoError(t, exitErr)
		var decoded agentadapter.HookOutput
		require.NoError(t, json.Unmarshal([]byte(out), &decoded), "got: %s", out)
		assert.Equal(t, agentadapter.DecisionDeny, decoded.HookSpecificOutput.PermissionDecision)
	})

	t.Run("non_bash_tool_untouched", func(t *testing.T) {
		out, exitErr := runMgitTestStdin(t, bin, repo,
			`{"hook_event_name":"PreToolUse","tool_name":"Read","cwd":"`+repo+`"}`,
			"sandbox", "codex-hook")
		assert.NoError(t, exitErr)
		assert.JSONEq(t, "{}", out, "a non-Bash tool call must pass through untouched")
	})
}

func TestCursorHook_RealProcess_UncontainedCommandIsRefused(t *testing.T) {
	bin := buildMgitTestBinary(t)
	repo := t.TempDir()
	require.NoError(t, runMgitTest(t, bin, repo, "init"))

	// The experiment from MGIT-149's description, in the shape Cursor sends it:
	// a command that resets PATH so the shims cannot be reached. Under the old
	// world this ran on the host, silently. It must now be refused.
	t.Run("the_path_reset_escape_is_refused", func(t *testing.T) {
		payload, err := json.Marshal(map[string]any{
			"command": `env PATH="/usr/bin:/bin" sh -c 'whoami; hostname -s; touch /tmp/PROOF'`,
			"cwd":     repo,
		})
		require.NoError(t, err)

		out, exitErr := runMgitTestStdin(t, bin, repo, string(payload), "sandbox", "cursor-hook")
		assert.NoError(t, exitErr)

		var decoded agentadapter.CursorHookOutput
		require.NoError(t, json.Unmarshal([]byte(out), &decoded), "got: %s", out)
		assert.Equal(t, agentadapter.CursorDeny, decoded.Permission)
		assert.Contains(t, decoded.AgentMessage, "mgit run")
	})

	t.Run("reply_uses_cursors_snake_case_keys", func(t *testing.T) {
		out, exitErr := runMgitTestStdin(t, bin, repo,
			`{"command":"go build ./...","cwd":"`+repo+`"}`, "sandbox", "cursor-hook")
		assert.NoError(t, exitErr)
		var raw map[string]any
		require.NoError(t, json.Unmarshal([]byte(out), &raw), "got: %s", out)
		assert.Contains(t, raw, "permission")
		assert.Contains(t, raw, "agent_message")
		assert.NotContains(t, raw, "agentMessage", "camelCase would be ignored by Cursor — failing open")
	})
}
