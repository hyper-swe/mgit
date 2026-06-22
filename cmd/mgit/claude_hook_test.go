package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runHook drives `mgit sandbox claude-hook` with the given stdin payload,
// an injected connector, and a HOME, capturing the JSON reply.
func runHook(connect connectFunc, home, stdin string) (map[string]any, error) {
	cmd := sandboxClaudeHookCmd(connect)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetIn(bytes.NewBufferString(stdin))
	cmd.SetArgs(nil)
	// Inject HOME via the production handler's homeFn by overriding the env.
	old, had := os.LookupEnv("HOME")
	_ = os.Setenv("HOME", home)
	defer func() {
		if had {
			_ = os.Setenv("HOME", old)
		} else {
			_ = os.Unsetenv("HOME")
		}
	}()
	err := cmd.Execute()
	if out.Len() == 0 {
		return nil, err
	}
	var decoded map[string]any
	if derr := json.Unmarshal(out.Bytes(), &decoded); derr != nil {
		return nil, derr
	}
	return decoded, err
}

func hookInput(tool, command, cwd string) string {
	in := map[string]any{
		"hook_event_name": "PreToolUse",
		"tool_name":       tool,
		"cwd":             cwd,
		"tool_input":      map[string]any{"command": command},
	}
	b, _ := json.Marshal(in)
	return string(b)
}

func decisionOf(t *testing.T, m map[string]any) (string, map[string]any) {
	t.Helper()
	hso, ok := m["hookSpecificOutput"].(map[string]any)
	require.True(t, ok, "reply has hookSpecificOutput: %v", m)
	dec, _ := hso["permissionDecision"].(string)
	upd, _ := hso["updatedInput"].(map[string]any)
	return dec, upd
}

// TestClaudeHook_Healthy_AllowsAndRewrites verifies the handler approves
// and rewrites a Bash command when a sandbox is bound to the cwd.
// Refs: MGIT-11.11.1
func TestClaudeHook_Healthy_AllowsAndRewrites(t *testing.T) {
	wt := filepath.FromSlash("/repo/wt")
	fc := &fakeSandboxClient{listResult: boundSandbox(wt, "MGIT-7.1")}

	reply, err := runHook(okConnect(fc), t.TempDir(), hookInput("Bash", "npm test", wt))

	require.NoError(t, err)
	dec, upd := decisionOf(t, reply)
	assert.Equal(t, "allow", dec)
	assert.Equal(t, `mgit run -- bash -lc 'npm test'`, upd["command"])
}

// TestClaudeHook_SandboxDown_Asks verifies an unreachable daemon yields
// 'ask' and no rewrite. Refs: MGIT-11.11.1, NFR-17.6
func TestClaudeHook_SandboxDown_Asks(t *testing.T) {
	reply, err := runHook(errConnect(errors.New("daemon down")), t.TempDir(),
		hookInput("Bash", "npm test", filepath.FromSlash("/repo/wt")))

	require.NoError(t, err)
	dec, upd := decisionOf(t, reply)
	assert.Equal(t, "ask", dec)
	assert.Nil(t, upd)
}

// TestClaudeHook_NoSandboxForCwd_Asks verifies that a reachable daemon
// with no sandbox bound to the cwd still fails closed to 'ask'.
// Refs: MGIT-11.11.1
func TestClaudeHook_NoSandboxForCwd_Asks(t *testing.T) {
	fc := &fakeSandboxClient{listResult: boundSandbox(filepath.FromSlash("/other"), "OTHER")}
	reply, err := runHook(okConnect(fc), t.TempDir(),
		hookInput("Bash", "npm test", filepath.FromSlash("/repo/wt")))
	require.NoError(t, err)
	dec, _ := decisionOf(t, reply)
	assert.Equal(t, "ask", dec)
}

// TestClaudeHook_UserDenyRule_Asks verifies a command matching a deny rule
// in the worktree settings is not auto-approved. Refs: MGIT-11.11.1
func TestClaudeHook_UserDenyRule_Asks(t *testing.T) {
	wt := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(wt, ".claude"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(wt, ".claude", "settings.json"),
		[]byte(`{"permissions":{"deny":["Bash(rm:*)"]}}`), 0o600))
	fc := &fakeSandboxClient{listResult: boundSandbox(wt, "MGIT-7.1")}

	reply, err := runHook(okConnect(fc), t.TempDir(), hookInput("Bash", "rm -rf /tmp/x", wt))

	require.NoError(t, err)
	dec, upd := decisionOf(t, reply)
	assert.Equal(t, "ask", dec, "denied command must not be auto-approved")
	assert.Nil(t, upd)
}

// TestClaudeHook_NonBash_NoInterference verifies non-Bash tools get an
// empty (no-decision) reply so the normal flow is untouched.
// Refs: MGIT-11.11.1
func TestClaudeHook_NonBash_NoInterference(t *testing.T) {
	fc := &fakeSandboxClient{listResult: boundSandbox(filepath.FromSlash("/repo/wt"), "MGIT-7.1")}
	reply, err := runHook(okConnect(fc), t.TempDir(),
		hookInput("Read", "", filepath.FromSlash("/repo/wt")))
	require.NoError(t, err)
	_, ok := reply["hookSpecificOutput"]
	assert.False(t, ok, "no permission decision emitted for non-Bash tools")
}

// TestClaudeHook_MalformedStdin_Asks verifies unparseable input defers to
// the prompt rather than guessing. Refs: MGIT-11.11.1
func TestClaudeHook_MalformedStdin_Asks(t *testing.T) {
	fc := &fakeSandboxClient{}
	reply, err := runHook(okConnect(fc), t.TempDir(), "{not json")
	require.NoError(t, err)
	dec, _ := decisionOf(t, reply)
	assert.Equal(t, "ask", dec)
}

// TestClaudeHook_EmptyCwd_FallsBackToGetwd verifies a missing cwd in the
// payload falls back to the process cwd (and still fails closed when no
// sandbox is bound there). Refs: MGIT-11.11.1
func TestClaudeHook_EmptyCwd_FallsBackToGetwd(t *testing.T) {
	fc := &fakeSandboxClient{listResult: boundSandbox(filepath.FromSlash("/nowhere"), "X")}
	reply, err := runHook(okConnect(fc), t.TempDir(), hookInput("Bash", "ls", ""))
	require.NoError(t, err)
	dec, _ := decisionOf(t, reply)
	assert.Equal(t, "ask", dec)
}

// TestInjectAgentAdapters_WritesClaudeSettings verifies worktree creation
// drops a routing-enabled settings.json into the worktree. Refs: MGIT-11.11.1
func TestInjectAgentAdapters_WritesClaudeSettings(t *testing.T) {
	wt := t.TempDir()
	var warn bytes.Buffer
	injectAgentAdapters(&warn, wt)

	b, err := os.ReadFile(filepath.Join(wt, ".claude", "settings.json")) //nolint:gosec // test-owned temp path
	require.NoError(t, err)
	assert.Contains(t, string(b), claudeHookCommand)
	assert.Empty(t, warn.String(), "no warning on success")
}

// TestInjectAgentAdapters_WarnsOnFailure verifies an unwritable target
// surfaces a warning without panicking. Refs: MGIT-11.11.1
func TestInjectAgentAdapters_WarnsOnFailure(t *testing.T) {
	// A path whose parent is a regular file cannot host a .claude dir.
	file := filepath.Join(t.TempDir(), "afile")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o600))
	var warn bytes.Buffer
	injectAgentAdapters(&warn, file)
	assert.Contains(t, warn.String(), "warning")
}
