package mcp

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	mcpclient "github.com/mark3labs/mcp-go/client"
	mcptypes "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newInProcess drives the REAL MCP server through an in-process client — the
// same dispatch (initialize → tools/list → tools/call), middleware, and result
// encoding an agent hits over stdio — instead of calling handlers directly.
// Refs: MGIT-50
func newInProcess(t *testing.T) (*mcpclient.Client, context.Context) {
	t.Helper()
	srv := setupTestMCP(t)
	c, err := mcpclient.NewInProcessClient(srv.MCPServer())
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	ctx := context.Background()
	require.NoError(t, c.Start(ctx))
	_, err = c.Initialize(ctx, mcptypes.InitializeRequest{})
	require.NoError(t, err)
	return c, ctx
}

func callTool(ctx context.Context, c *mcpclient.Client, name string, args map[string]any) (*mcptypes.CallToolResult, error) {
	req := mcptypes.CallToolRequest{}
	req.Params.Name = name
	req.Params.Arguments = args
	return c.CallTool(ctx, req)
}

func inProcResultText(t *testing.T, r *mcptypes.CallToolResult) string {
	t.Helper()
	for _, ct := range r.Content {
		if tc, ok := mcptypes.AsTextContent(ct); ok {
			return tc.Text
		}
	}
	return ""
}

// TestGA_ToolSurface_MatchesDocumented proves every documented tool is
// registered on the live server and none advertises placeholder text — driven
// through the real tools/list, not the handler map. Refs: MGIT-50, MGIT-45
func TestGA_ToolSurface_MatchesDocumented(t *testing.T) {
	c, ctx := newInProcess(t)
	res, err := c.ListTools(ctx, mcptypes.ListToolsRequest{})
	require.NoError(t, err)

	have := map[string]bool{}
	for _, tool := range res.Tools {
		have[tool.Name] = true
		assert.NotContains(t, strings.ToLower(tool.Description), "not yet available",
			"tool %s advertises placeholder text", tool.Name)
	}
	documented := []string{
		"mgit_commit", "mgit_rollback", "mgit_squash", "mgit_status", "mgit_log",
		"mgit_show", "mgit_branch", "mgit_verify", "mgit_diff", "mgit_export",
		"mgit_audit", "mgit_config",
		"mgit_worktree_add", "mgit_worktree_list", "mgit_worktree_remove",
	}
	for _, name := range documented {
		assert.True(t, have[name], "documented tool %q not registered", name)
	}
}

// TestGA_HostileInput_Rejected treats MCP input as hostile: oversized payloads,
// control characters, path traversal, and malformed task ids must all come back
// as structured tool errors, not crashes or laundered operations. Refs: MGIT-50
func TestGA_HostileInput_Rejected(t *testing.T) {
	c, ctx := newInProcess(t)
	huge := strings.Repeat("a", maxArgLen+1)

	tests := []struct {
		name string
		tool string
		args map[string]any
	}{
		{"commit malformed task_id", "mgit_commit", map[string]any{"task_id": "not a task!!", "message": "x"}},
		{"commit control char in task_id", "mgit_commit", map[string]any{"task_id": "MGIT-1\x00", "message": "x"}},
		{"commit oversized message", "mgit_commit", map[string]any{"task_id": "MGIT-1.1", "message": huge}},
		{"commit NUL in message", "mgit_commit", map[string]any{"task_id": "MGIT-1.1", "message": "a\x00b"}},
		{"worktree_add traversal path", "mgit_worktree_add", map[string]any{"path": "/tmp/../etc/x", "task_id": "MGIT-1.1"}},
		{"worktree_add control-char path", "mgit_worktree_add", map[string]any{"path": "/tmp/a\x01b", "task_id": "MGIT-1.1"}},
		{"worktree_add oversized path", "mgit_worktree_add", map[string]any{"path": huge, "task_id": "MGIT-1.1"}},
		{"worktree_add malformed task_id", "mgit_worktree_add", map[string]any{"path": "/tmp/wt", "task_id": "../evil"}},
		{"worktree_remove traversal path", "mgit_worktree_remove", map[string]any{"path": "a/../../b"}},
		{"show control-char commit_id", "mgit_show", map[string]any{"commit_id": "abc\x00"}},
		{"export malformed task_id", "mgit_export", map[string]any{"task_id": "DROP TABLE"}},
		{"log malformed task_id", "mgit_log", map[string]any{"task_id": "a b c"}},
		{"rollback malformed task_id", "mgit_rollback", map[string]any{"task_id": "bad id"}},
		{"squash malformed task_id", "mgit_squash", map[string]any{"task_id": "../x"}},
		{"branch malformed task_id", "mgit_branch", map[string]any{"task_id": "a;b"}},
		{"config oversized key", "mgit_config", map[string]any{"key": strings.Repeat("k", maxArgLen+1)}},
		{"diff malformed task_id", "mgit_diff", map[string]any{"task_id": "no good"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := callTool(ctx, c, tt.tool, tt.args)
			require.NoError(t, err, "transport error (should be a tool-level error instead)")
			assert.True(t, res.IsError, "hostile input must be rejected as a tool error")
			assert.NotContains(t, inProcResultText(t, res), "panic")
		})
	}
}

// TestGA_WorktreeLifecycle_ThroughServer runs the worktree tools end to end via
// the real server and asserts real materialization. Refs: MGIT-50, MGIT-45
func TestGA_WorktreeLifecycle_ThroughServer(t *testing.T) {
	c, ctx := newInProcess(t)
	wt := filepath.Join(t.TempDir(), "ga-wt")

	res, err := callTool(ctx, c, "mgit_worktree_add", map[string]any{"path": wt, "task_id": "GA-1"})
	require.NoError(t, err)
	require.False(t, res.IsError, inProcResultText(t, res))

	res, err = callTool(ctx, c, "mgit_worktree_list", nil)
	require.NoError(t, err)
	assert.Contains(t, inProcResultText(t, res), "GA-1")

	res, err = callTool(ctx, c, "mgit_worktree_remove", map[string]any{"path": wt})
	require.NoError(t, err)
	assert.False(t, res.IsError)
}

// TestGA_ReadTools_ReturnRealData proves the formerly-stubbed read tools now
// return real data through the server (no canned placeholder). Refs: MGIT-50
func TestGA_ReadTools_ReturnRealData(t *testing.T) {
	c, ctx := newInProcess(t)

	for _, tc := range []struct {
		tool, notWant string
	}{
		{"mgit_status", "working tree clean"},
		{"mgit_audit", "no audit entries"},
		{"mgit_config", "config: default"},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			res, err := callTool(ctx, c, tc.tool, nil)
			require.NoError(t, err)
			require.False(t, res.IsError, inProcResultText(t, res))
			assert.NotContains(t, inProcResultText(t, res), tc.notWant,
				"%s must return real data, not the old placeholder", tc.tool)
		})
	}
}
