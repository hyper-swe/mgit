package agentadapter

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHook_AutoApprovesWhenHealthy verifies a healthy sandbox yields an
// allow decision. Refs: MGIT-11.11.1
func TestHook_AutoApprovesWhenHealthy(t *testing.T) {
	in := HookInput{ToolName: "Bash"}
	in.ToolInput.Command = "npm test"

	out := Decide(in, true, false)

	assert.Equal(t, "PreToolUse", out.HookSpecificOutput.HookEventName)
	assert.Equal(t, DecisionAllow, out.HookSpecificOutput.PermissionDecision)
}

// TestHook_ReroutesBashToGuest verifies the auto-approved command is
// rewritten to route through `mgit run` into the guest. Refs: MGIT-11.11.1
func TestHook_ReroutesBashToGuest(t *testing.T) {
	in := HookInput{ToolName: "Bash"}
	in.ToolInput.Command = "npm test"

	out := Decide(in, true, false)

	rewritten, ok := out.HookSpecificOutput.UpdatedInput["command"].(string)
	require.True(t, ok, "updatedInput.command must be a string")
	assert.Equal(t, `mgit run -- bash -lc 'npm test'`, rewritten)
}

// TestHook_RewriteEscapesSingleQuotes verifies a command containing a
// single quote is safely wrapped for `bash -lc`. Refs: MGIT-11.11.1
func TestHook_RewriteEscapesSingleQuotes(t *testing.T) {
	got := RewriteCommand(`echo 'hi there'`)
	// The inner quotes are closed, escaped, and reopened: '\'' per token.
	assert.Equal(t, `mgit run -- bash -lc 'echo '\''hi there'\'''`, got)
}

// TestHook_SandboxDown_FailsClosedAsk verifies an unavailable sandbox
// yields 'ask' (the normal prompt resumes) and NO rewrite — the command
// is never silently auto-approved onto the host. Refs: MGIT-11.11.1, NFR-17.6
func TestHook_SandboxDown_FailsClosedAsk(t *testing.T) {
	in := HookInput{ToolName: "Bash"}
	in.ToolInput.Command = "npm test"

	out := Decide(in, false, false)

	assert.Equal(t, DecisionAsk, out.HookSpecificOutput.PermissionDecision)
	assert.Nil(t, out.HookSpecificOutput.UpdatedInput, "no rewrite when sandbox is down")
}

// TestHook_RespectsUserDenyRules verifies that a command matching a user
// deny rule is never auto-approved/rewritten even when the sandbox is
// healthy — it defers to the normal flow so the deny still blocks.
// Refs: MGIT-11.11.1
func TestHook_RespectsUserDenyRules(t *testing.T) {
	rules := []string{"Bash(rm:*)"}
	require.True(t, CommandDenied("rm -rf /tmp/x", rules), "deny rule must match")

	in := HookInput{ToolName: "Bash"}
	in.ToolInput.Command = "rm -rf /tmp/x"
	out := Decide(in, true, CommandDenied(in.ToolInput.Command, rules))

	assert.Equal(t, DecisionAsk, out.HookSpecificOutput.PermissionDecision)
	assert.Nil(t, out.HookSpecificOutput.UpdatedInput, "denied command must not be rewritten/laundered")
}

// TestClaudeSettings_HasBashPreToolUseHook verifies generated settings
// register a Bash-matched PreToolUse command hook. Refs: MGIT-11.11.1
func TestClaudeSettings_HasBashPreToolUseHook(t *testing.T) {
	b, err := MergeClaudeSettings(nil, "mgit sandbox claude-hook")
	require.NoError(t, err)

	var s map[string]any
	require.NoError(t, json.Unmarshal(b, &s))
	hooks := s["hooks"].(map[string]any)
	pre := hooks["PreToolUse"].([]any)
	require.Len(t, pre, 1)
	entry := pre[0].(map[string]any)
	assert.Equal(t, "Bash", entry["matcher"])
	inner := entry["hooks"].([]any)[0].(map[string]any)
	assert.Equal(t, "command", inner["type"])
	assert.Equal(t, "mgit sandbox claude-hook", inner["command"])
}

// TestClaudeSettings_AllowsMgitRun verifies generated settings whitelist
// the rewritten `mgit run` invocation so it does not re-prompt.
// Refs: MGIT-11.11.1
func TestClaudeSettings_AllowsMgitRun(t *testing.T) {
	b, err := MergeClaudeSettings(nil, "mgit sandbox claude-hook")
	require.NoError(t, err)

	var s map[string]any
	require.NoError(t, json.Unmarshal(b, &s))
	perms := s["permissions"].(map[string]any)
	allow := perms["allow"].([]any)
	assert.Contains(t, allow, "Bash(mgit run:*)")
}

// TestMergeClaudeSettings_PreservesExisting verifies merging into an
// existing settings.json keeps unrelated user keys and is idempotent.
// Refs: MGIT-11.11.1
func TestMergeClaudeSettings_PreservesExisting(t *testing.T) {
	existing := []byte(`{"model":"opus","permissions":{"allow":["Read(*)"],"deny":["Bash(curl:*)"]}}`)

	b, err := MergeClaudeSettings(existing, "mgit sandbox claude-hook")
	require.NoError(t, err)

	var s map[string]any
	require.NoError(t, json.Unmarshal(b, &s))
	assert.Equal(t, "opus", s["model"], "unrelated keys preserved")
	perms := s["permissions"].(map[string]any)
	allow := toStrings(perms["allow"].([]any))
	assert.Contains(t, allow, "Read(*)", "existing allow preserved")
	assert.Contains(t, allow, "Bash(mgit run:*)", "our allow added")
	deny := toStrings(perms["deny"].([]any))
	assert.Contains(t, deny, "Bash(curl:*)", "existing deny preserved")

	// Idempotent: merging again adds no duplicates.
	b2, err := MergeClaudeSettings(b, "mgit sandbox claude-hook")
	require.NoError(t, err)
	var s2 map[string]any
	require.NoError(t, json.Unmarshal(b2, &s2))
	pre := s2["hooks"].(map[string]any)["PreToolUse"].([]any)
	assert.Len(t, pre, 1, "no duplicate hook entry on re-merge")
	allow2 := toStrings(s2["permissions"].(map[string]any)["allow"].([]any))
	assert.Equal(t, 1, count(allow2, "Bash(mgit run:*)"), "no duplicate allow on re-merge")
}

// TestWriteClaudeSettings_WorktreeScoped verifies the settings file is
// written under the worktree's own .claude directory. Refs: MGIT-11.11.1
func TestWriteClaudeSettings_WorktreeScoped(t *testing.T) {
	wt := t.TempDir()
	require.NoError(t, WriteClaudeSettings(wt, "mgit sandbox claude-hook"))

	path := filepath.Join(wt, ".claude", "settings.json")
	b, err := os.ReadFile(path) //nolint:gosec // test-owned temp path
	require.NoError(t, err)
	assert.True(t, strings.Contains(string(b), "PreToolUse"))
}

// TestMergeClaudeSettings_InvalidExisting_Errors verifies a corrupt
// settings.json is reported, not silently overwritten. Refs: MGIT-11.11.1
func TestMergeClaudeSettings_InvalidExisting_Errors(t *testing.T) {
	_, err := MergeClaudeSettings([]byte("{not json"), "mgit sandbox claude-hook")
	assert.Error(t, err)
}

// TestWriteClaudeSettings_MergesExistingFile verifies an on-disk
// settings.json is merged in place, preserving prior content.
// Refs: MGIT-11.11.1
func TestWriteClaudeSettings_MergesExistingFile(t *testing.T) {
	wt := t.TempDir()
	dir := filepath.Join(wt, ".claude")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	path := filepath.Join(dir, "settings.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"model":"opus"}`), 0o600))

	require.NoError(t, WriteClaudeSettings(wt, "mgit sandbox claude-hook"))

	b, err := os.ReadFile(path) //nolint:gosec // test-owned temp path
	require.NoError(t, err)
	var s map[string]any
	require.NoError(t, json.Unmarshal(b, &s))
	assert.Equal(t, "opus", s["model"], "prior content preserved")
	assert.Contains(t, string(b), "PreToolUse", "hook merged in")
}

func toStrings(in []any) []string {
	out := make([]string, len(in))
	for i, v := range in {
		out[i], _ = v.(string)
	}
	return out
}

func count(in []string, target string) int {
	n := 0
	for _, v := range in {
		if v == target {
			n++
		}
	}
	return n
}
