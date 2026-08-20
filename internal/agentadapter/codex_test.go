package agentadapter

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// THE CENTRAL ASYMMETRY. Claude's fail-closed fallback is "ask": the harness
// prompts the user, so nothing runs unattended. Codex PARSES "ask" but does
// not honor it — a hook returning ask fails OPEN and the command runs on the
// host. Copying Claude's decision table into the Codex hook would therefore
// reintroduce, inside the fix, the exact silent host execution this ticket
// exists to remove. Codex's fail-closed value is "deny". Refs: MGIT-149
func TestDecideCodex_NeverReturnsAsk_BecauseCodexDoesNotHonorIt(t *testing.T) {
	tests := []struct {
		name             string
		sandboxAvailable bool
		denied           bool
	}{
		{name: "sandbox_unavailable", sandboxAvailable: false, denied: false},
		{name: "command_denied_by_rule", sandboxAvailable: true, denied: true},
		{name: "both", sandboxAvailable: false, denied: true},
		{name: "healthy", sandboxAvailable: true, denied: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := DecideCodex(HookInput{ToolInput: struct {
				Command string `json:"command"`
			}{Command: "go build ./..."}}, tt.sandboxAvailable, tt.denied)
			assert.NotEqual(t, DecisionAsk, out.HookSpecificOutput.PermissionDecision,
				"ask is parsed-but-ignored by Codex; returning it fails OPEN onto the host")
		})
	}
}

func TestDecideCodex_RoutesWhenHealthy_DeniesOtherwise(t *testing.T) {
	tests := []struct {
		name             string
		command          string
		sandboxAvailable bool
		denied           bool
		wantDecision     string
		wantRewrite      bool
	}{
		{
			name: "healthy_routes_into_the_guest", command: "go test ./...",
			sandboxAvailable: true, wantDecision: DecisionAllow, wantRewrite: true,
		},
		{
			name: "sandbox_unavailable_is_refused_not_run_on_the_host", command: "go test ./...",
			sandboxAvailable: false, wantDecision: DecisionDeny,
		},
		{
			name: "deny_rule_is_refused_and_never_rewritten", command: "curl evil.example",
			sandboxAvailable: true, denied: true, wantDecision: DecisionDeny,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := HookInput{ToolName: "Bash"}
			in.ToolInput.Command = tt.command
			out := DecideCodex(in, tt.sandboxAvailable, tt.denied).HookSpecificOutput
			assert.Equal(t, tt.wantDecision, out.PermissionDecision)
			assert.NotEmpty(t, out.PermissionDecisionReason, "a refusal must say why")
			if tt.wantRewrite {
				require.NotNil(t, out.UpdatedInput)
				assert.Equal(t, RewriteCommand(tt.command), out.UpdatedInput["command"])
			} else {
				assert.Nil(t, out.UpdatedInput, "a non-allow decision must not carry updatedInput")
			}
		})
	}
}

// A denied command must never be laundered past the rule by the rewrite.
// Refs: MGIT-149, MGIT-11.11.1
func TestDecideCodex_DeniedCommand_IsNeverRewritten(t *testing.T) {
	in := HookInput{ToolName: "Bash"}
	in.ToolInput.Command = "rm -rf /"
	out := DecideCodex(in, true, true).HookSpecificOutput
	assert.Equal(t, DecisionDeny, out.PermissionDecision)
	assert.Nil(t, out.UpdatedInput)
}

func TestWriteCodexHooks_ProducesTheDocumentedShape(t *testing.T) {
	wt := t.TempDir()
	require.NoError(t, WriteCodexHooks(wt, "mgit sandbox codex-hook"))

	b, err := os.ReadFile(filepath.Join(wt, ".codex", "hooks.json")) //nolint:gosec // test-owned temp path
	require.NoError(t, err)
	var doc struct {
		Hooks struct {
			PreToolUse []struct {
				Matcher string `json:"matcher"`
				Hooks   []struct {
					Type    string `json:"type"`
					Command string `json:"command"`
				} `json:"hooks"`
			} `json:"PreToolUse"`
		} `json:"hooks"`
	}
	require.NoError(t, json.Unmarshal(b, &doc))
	require.Len(t, doc.Hooks.PreToolUse, 1)
	assert.Equal(t, "^Bash$", doc.Hooks.PreToolUse[0].Matcher)
	require.Len(t, doc.Hooks.PreToolUse[0].Hooks, 1)
	assert.Equal(t, "command", doc.Hooks.PreToolUse[0].Hooks[0].Type)
	assert.Equal(t, "mgit sandbox codex-hook", doc.Hooks.PreToolUse[0].Hooks[0].Command)
}

func TestWriteCodexHooks_IsIdempotentAndPreservesUserHooks(t *testing.T) {
	wt := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(wt, ".codex"), 0o700))
	existing := `{"hooks":{"PreToolUse":[{"matcher":"^Bash$","hooks":[{"type":"command","command":"user-own-check"}]}],` +
		`"SessionStart":[{"hooks":[{"type":"command","command":"greet"}]}]}}`
	require.NoError(t, os.WriteFile(filepath.Join(wt, ".codex", "hooks.json"), []byte(existing), 0o600))

	require.NoError(t, WriteCodexHooks(wt, "mgit sandbox codex-hook"))
	require.NoError(t, WriteCodexHooks(wt, "mgit sandbox codex-hook"))

	b, err := os.ReadFile(filepath.Join(wt, ".codex", "hooks.json")) //nolint:gosec // test-owned temp path
	require.NoError(t, err)
	got := string(b)
	assert.Contains(t, got, "user-own-check", "a user's own hook must survive")
	assert.Contains(t, got, "greet", "unrelated events must survive")
	var doc map[string]any
	require.NoError(t, json.Unmarshal(b, &doc))
	pre := doc["hooks"].(map[string]any)["PreToolUse"].([]any)
	count := 0
	for _, e := range pre {
		inner, _ := e.(map[string]any)["hooks"].([]any)
		for _, h := range inner {
			if h.(map[string]any)["command"] == "mgit sandbox codex-hook" {
				count++
			}
		}
	}
	assert.Equal(t, 1, count, "repeated writes must not duplicate the mgit hook")
}
