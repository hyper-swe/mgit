package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestToolDocs_CoversRegisteredSurface guards the no-drift contract (MGIT-50):
// ToolDocs() — the source `mgit docs generate` documents from — must enumerate
// every registered tool with a real (non-empty, non-placeholder) description
// and its parameters. If a tool is added/removed/renamed, the generated MCP
// reference changes automatically and this test's count assertion flags a
// mismatch with the documented contract.
func TestToolDocs_CoversRegisteredSurface(t *testing.T) {
	srv := setupTestMCP(t)
	docs := srv.ToolDocs()

	want := map[string][]string{
		"mgit_commit":          {"agent_id", "message", "task_id"},
		"mgit_rollback":        {"dry_run", "reason", "task_id"},
		"mgit_squash":          {"dry_run", "message", "task_id"},
		"mgit_status":          {},
		"mgit_log":             {"limit", "task_id"},
		"mgit_show":            {"commit_id"},
		"mgit_branch":          {"active_only", "task_id"},
		"mgit_verify":          {"task_id"},
		"mgit_diff":            {"commit1", "commit2", "task_id"},
		"mgit_export":          {"task_id"},
		"mgit_audit":           {"task_id"},
		"mgit_config":          {"key", "value"},
		"mgit_worktree_add":    {"agent_id", "branch", "path", "task_id"},
		"mgit_worktree_list":   {},
		"mgit_worktree_remove": {"force", "path"},
	}

	require.Len(t, docs, len(want), "ToolDocs must cover exactly the documented tool set")
	seen := map[string]bool{}
	for _, d := range docs {
		seen[d.Name] = true
		wantParams, ok := want[d.Name]
		assert.True(t, ok, "undocumented tool registered: %s", d.Name)
		assert.NotEmpty(t, d.Description, "tool %s has no description", d.Name)
		assert.NotContains(t, d.Description, "not yet available", "tool %s advertises a placeholder", d.Name)
		assert.Equal(t, wantParams, d.Parameters, "tool %s parameter set drifted", d.Name)
	}
	for name := range want {
		assert.True(t, seen[name], "documented tool %s is no longer registered", name)
	}
}
