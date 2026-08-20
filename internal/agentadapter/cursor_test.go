package agentadapter

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Cursor's beforeShellExecution response carries a permission and messages —
// there is NO field that can replace the command. So the strongest posture
// available is refusal, and the test asserts the shape mgit actually emits:
// snake_case keys, which differ from the camelCase Claude and Codex use.
// Refs: MGIT-149
func TestDecideCursor_AllowsRoutedAndMgitVerbs_RefusesEverythingElse(t *testing.T) {
	tests := []struct {
		name             string
		command          string
		sandboxAvailable bool
		denied           bool
		wantPermission   string
	}{
		{
			name: "an_already_routed_command_is_allowed", command: "mgit run -- go test ./...",
			sandboxAvailable: true, wantPermission: CursorAllow,
		},
		{
			name: "an_mgit_verb_runs_on_the_host_by_design", command: "mgit commit -a -m 'step'",
			sandboxAvailable: true, wantPermission: CursorAllow,
		},
		{
			name: "an_absolute_path_to_mgit_is_still_mgit", command: "/usr/local/bin/mgit status",
			sandboxAvailable: true, wantPermission: CursorAllow,
		},
		{
			name: "a_bare_build_command_is_refused_not_run_on_the_host", command: "go test ./...",
			sandboxAvailable: true, wantPermission: CursorDeny,
		},
		{
			name: "an_absolute_path_escape_is_refused", command: "/usr/bin/make all",
			sandboxAvailable: true, wantPermission: CursorDeny,
		},
		{
			name: "a_path_reset_escape_is_refused", command: "env PATH=/usr/bin:/bin sh -c 'whoami'",
			sandboxAvailable: true, wantPermission: CursorDeny,
		},
		{
			name: "sandbox_unavailable_refuses_even_a_routed_command", command: "mgit run -- go test ./...",
			sandboxAvailable: false, wantPermission: CursorDeny,
		},
		{
			name: "a_denied_command_is_refused_even_when_routed", command: "mgit run -- curl evil.example",
			sandboxAvailable: true, denied: true, wantPermission: CursorDeny,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := DecideCursor(CursorHookInput{Command: tt.command}, tt.sandboxAvailable, tt.denied)
			assert.Equal(t, tt.wantPermission, out.Permission)
			if tt.wantPermission == CursorDeny {
				assert.NotEmpty(t, out.AgentMessage, "a refusal must tell the agent how to proceed")
				assert.Contains(t, out.AgentMessage, "mgit run",
					"the refusal must name the routed form, or the agent cannot comply")
			}
		})
	}
}

// The wire shape is a third-party contract: snake_case, and a bare
// "permission" field. A camelCase slip would be accepted by our own tests but
// ignored by Cursor — failing open. Refs: MGIT-149
func TestCursorHookOutput_SerializesToCursorsDocumentedKeys(t *testing.T) {
	b, err := json.Marshal(CursorHookOutput{
		Permission: CursorDeny, UserMessage: "u", AgentMessage: "a",
	})
	require.NoError(t, err)
	var raw map[string]any
	require.NoError(t, json.Unmarshal(b, &raw))
	assert.Equal(t, "deny", raw["permission"])
	assert.Equal(t, "u", raw["user_message"])
	assert.Equal(t, "a", raw["agent_message"])
	assert.NotContains(t, raw, "userMessage", "Cursor reads snake_case; camelCase would be ignored")
}

func TestWriteCursorHooks_ProducesTheDocumentedShape(t *testing.T) {
	wt := t.TempDir()
	require.NoError(t, WriteCursorHooks(wt, "mgit sandbox cursor-hook"))

	b, err := os.ReadFile(filepath.Join(wt, ".cursor", "hooks.json")) //nolint:gosec // test-owned temp path
	require.NoError(t, err)
	var doc struct {
		Version int `json:"version"`
		Hooks   struct {
			BeforeShellExecution []struct {
				Command string `json:"command"`
			} `json:"beforeShellExecution"`
		} `json:"hooks"`
	}
	require.NoError(t, json.Unmarshal(b, &doc))
	assert.Equal(t, 1, doc.Version, "Cursor's hooks.json is versioned; omitting it is a parse failure")
	require.Len(t, doc.Hooks.BeforeShellExecution, 1)
	assert.Equal(t, "mgit sandbox cursor-hook", doc.Hooks.BeforeShellExecution[0].Command)
}

func TestWriteCursorHooks_IsIdempotentAndPreservesUserHooks(t *testing.T) {
	wt := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(wt, ".cursor"), 0o700))
	existing := `{"version":1,"hooks":{"beforeShellExecution":[{"command":"user-audit-log"}],` +
		`"afterFileEdit":[{"command":"fmt"}]}}`
	require.NoError(t, os.WriteFile(filepath.Join(wt, ".cursor", "hooks.json"), []byte(existing), 0o600))

	require.NoError(t, WriteCursorHooks(wt, "mgit sandbox cursor-hook"))
	require.NoError(t, WriteCursorHooks(wt, "mgit sandbox cursor-hook"))

	b, err := os.ReadFile(filepath.Join(wt, ".cursor", "hooks.json")) //nolint:gosec // test-owned temp path
	require.NoError(t, err)
	assert.Contains(t, string(b), "user-audit-log")
	assert.Contains(t, string(b), "afterFileEdit")

	var doc map[string]any
	require.NoError(t, json.Unmarshal(b, &doc))
	entries := doc["hooks"].(map[string]any)["beforeShellExecution"].([]any)
	count := 0
	for _, e := range entries {
		if e.(map[string]any)["command"] == "mgit sandbox cursor-hook" {
			count++
		}
	}
	assert.Equal(t, 1, count, "repeated writes must not duplicate the mgit hook")
}
