package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/controlproto"
)

// fakePolicyClient stands in for the sandbox daemon.
type fakePolicyClient struct {
	gotTask    string
	gotEntries []string
	gotDrain   bool
	showTask   string
	result     *controlproto.PolicyResult
	err        error
}

func (f *fakePolicyClient) SetEgressPolicy(
	_ context.Context, taskID string, entries []string, drain bool,
) (*controlproto.PolicyResult, error) {
	f.gotTask, f.gotEntries, f.gotDrain = taskID, entries, drain
	if f.err != nil {
		return nil, f.err
	}
	return f.result, nil
}

func (f *fakePolicyClient) EgressPolicy(
	_ context.Context, taskID string,
) (*controlproto.PolicyResult, error) {
	f.showTask = taskID
	if f.err != nil {
		return nil, f.err
	}
	return f.result, nil
}

// callPolicy invokes the tool handler directly with the given arguments.
func callPolicy(t *testing.T, srv *Server, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	req := mcp.CallToolRequest{}
	req.Params.Name = "mgit_sandbox_policy"
	req.Params.Arguments = args
	res, err := srv.sandboxPolicyTool(context.Background(), req)
	require.NoError(t, err, "tool handlers report failures as results, never as Go errors")
	return res
}

// TestSandboxPolicyTool_Revoke_KillsByDefault verifies an agent's revoke sends
// no entries and does NOT drain — the default has to be the safe one no matter
// which surface the caller uses. Refs: MGIT-72, ADR-012
func TestSandboxPolicyTool_Revoke_KillsByDefault(t *testing.T) {
	fc := &fakePolicyClient{result: &controlproto.PolicyResult{Killed: 2}}
	srv := setupTestMCP(t, WithSandboxPolicy(policyConnector(fc)))

	res := callPolicy(t, srv, map[string]any{"action": "revoke", "task_id": "MGIT-72"})

	require.False(t, res.IsError, resultText(t, res))
	assert.Equal(t, "MGIT-72", fc.gotTask)
	assert.Empty(t, fc.gotEntries)
	assert.False(t, fc.gotDrain, "kill is the default on MCP exactly as on the CLI")

	var got controlproto.PolicyResult
	require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &got))
	assert.Equal(t, 2, got.Killed)
}

// TestSandboxPolicyTool_Revoke_DrainIsOptIn is the matching positive control.
func TestSandboxPolicyTool_Revoke_DrainIsOptIn(t *testing.T) {
	fc := &fakePolicyClient{result: &controlproto.PolicyResult{Drained: true}}
	srv := setupTestMCP(t, WithSandboxPolicy(policyConnector(fc)))

	res := callPolicy(t, srv, map[string]any{
		"action": "revoke", "task_id": "MGIT-72", "drain": true,
	})

	require.False(t, res.IsError, resultText(t, res))
	assert.True(t, fc.gotDrain)
}

// TestSandboxPolicyTool_Set_CarriesTheAllowlist verifies a grant reaches the
// daemon as one atomic replacement. Refs: MGIT-72
func TestSandboxPolicyTool_Set_CarriesTheAllowlist(t *testing.T) {
	fc := &fakePolicyClient{result: &controlproto.PolicyResult{
		Entries: []string{"registry.npmjs.org:443"}, RuleCount: 1,
	}}
	srv := setupTestMCP(t, WithSandboxPolicy(policyConnector(fc)))

	res := callPolicy(t, srv, map[string]any{
		"action": "set", "task_id": "MGIT-72",
		"allow": []any{"registry.npmjs.org:443", "proxy.example:8080"},
	})

	require.False(t, res.IsError, resultText(t, res))
	assert.Equal(t, []string{"registry.npmjs.org:443", "proxy.example:8080"}, fc.gotEntries)
}

// TestSandboxPolicyTool_Show_ReportsTheLivePolicy verifies an agent can
// CONFIRM a revoke rather than take it on faith. Refs: MGIT-72
func TestSandboxPolicyTool_Show_ReportsTheLivePolicy(t *testing.T) {
	fc := &fakePolicyClient{result: &controlproto.PolicyResult{Entries: []string{"a.example:443"}}}
	srv := setupTestMCP(t, WithSandboxPolicy(policyConnector(fc)))

	res := callPolicy(t, srv, map[string]any{"action": "show", "task_id": "MGIT-72"})

	require.False(t, res.IsError, resultText(t, res))
	assert.Equal(t, "MGIT-72", fc.showTask)
	assert.Contains(t, resultText(t, res), "a.example:443")
}

// TestSandboxPolicyTool_SetWithoutAllow_IsRefused verifies `set` with no
// destinations is refused rather than silently revoking everything — the same
// guard the CLI has, because the hazard is the same on either surface.
// Refs: MGIT-72
func TestSandboxPolicyTool_SetWithoutAllow_IsRefused(t *testing.T) {
	fc := &fakePolicyClient{}
	srv := setupTestMCP(t, WithSandboxPolicy(policyConnector(fc)))

	res := callPolicy(t, srv, map[string]any{"action": "set", "task_id": "MGIT-72"})

	require.True(t, res.IsError)
	assert.Contains(t, resultText(t, res), "revoke")
	assert.Empty(t, fc.gotTask, "nothing may be applied when the intent is ambiguous")
}

// TestSandboxPolicyTool_RejectsBadInput verifies untrusted agent input is
// validated at the boundary: unknown actions, missing or malformed task IDs
// and non-string allow entries are refused before anything is applied.
func TestSandboxPolicyTool_RejectsBadInput(t *testing.T) {
	tests := []struct {
		name string
		args map[string]any
		want string
	}{
		{name: "missing_task", args: map[string]any{"action": "revoke"}, want: "task_id"},
		{name: "missing_action", args: map[string]any{"task_id": "MGIT-72"}, want: "action"},
		{
			name: "unknown_action",
			args: map[string]any{"action": "widen_everything", "task_id": "MGIT-72"},
			want: "action",
		},
		{
			name: "malformed_task",
			args: map[string]any{"action": "revoke", "task_id": "not a task!"},
			want: "task",
		},
		{
			name: "non_string_allow",
			args: map[string]any{"action": "set", "task_id": "MGIT-72", "allow": []any{42}},
			want: "allow",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fc := &fakePolicyClient{}
			srv := setupTestMCP(t, WithSandboxPolicy(policyConnector(fc)))

			res := callPolicy(t, srv, tt.args)

			require.True(t, res.IsError, resultText(t, res))
			assert.Contains(t, strings.ToLower(resultText(t, res)), tt.want)
			assert.Empty(t, fc.gotTask)
		})
	}
}

// TestSandboxPolicyTool_NoDaemonWired_SaysSo verifies the tool reports that
// the sandbox daemon is unavailable instead of returning a cheerful success.
//
// A revoke that reports success without reaching an enforcer is the worst
// failure this verb has: the agent then runs untrusted code believing egress
// is closed. Refs: MGIT-72, SEC-04
func TestSandboxPolicyTool_NoDaemonWired_SaysSo(t *testing.T) {
	srv := setupTestMCP(t) // no WithSandboxPolicy

	res := callPolicy(t, srv, map[string]any{"action": "revoke", "task_id": "MGIT-72"})

	require.True(t, res.IsError)
	assert.Contains(t, strings.ToLower(resultText(t, res)), "daemon")
}

// TestSandboxPolicyTool_DaemonError_IsSurfaced verifies a failure to reach the
// enforcer is reported as a tool error carrying the reason.
func TestSandboxPolicyTool_DaemonError_IsSurfaced(t *testing.T) {
	fc := &fakePolicyClient{err: errors.New("vm control channel unreachable")}
	srv := setupTestMCP(t, WithSandboxPolicy(policyConnector(fc)))

	res := callPolicy(t, srv, map[string]any{"action": "revoke", "task_id": "MGIT-72"})

	require.True(t, res.IsError)
	assert.Contains(t, resultText(t, res), "unreachable")
}

// policyConnector adapts a fake client to the connector the option takes.
func policyConnector(c SandboxPolicyClient) SandboxPolicyConnector {
	return func(context.Context) (SandboxPolicyClient, error) { return c, nil }
}
