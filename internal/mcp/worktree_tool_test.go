package mcp

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	mcptypes "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gitstore "github.com/hyper-swe/mgit/internal/store/git"
	"github.com/hyper-swe/mgit/internal/store/index"
)

// resultText extracts the first text content of a tool result, failing the
// test if the result carries none.
func resultText(t *testing.T, r *mcptypes.CallToolResult) string {
	t.Helper()
	require.NotEmpty(t, r.Content, "tool result has no content")
	tc, ok := mcptypes.AsTextContent(r.Content[0])
	require.True(t, ok, "first content is not text")
	return tc.Text
}

// TestMCP_WorktreeAddTool_CreatesRealWorktree proves the tool delegates to the
// real WorktreeService: it materializes a worktree on disk and the worktree
// shows up in the registry — no placeholder text. Refs: MGIT-45, FR-16
func TestMCP_WorktreeAddTool_CreatesRealWorktree(t *testing.T) {
	srv := setupTestMCP(t)
	ctx := context.Background()

	// A base commit so the task branch has a head to fork from.
	_, err := srv.commitTool(ctx, makeToolReq(map[string]any{
		"task_id": "MGIT-9.1", "message": "base",
	}))
	require.NoError(t, err)

	dest := filepath.Join(t.TempDir(), "wt-9-2")
	res, err := srv.worktreeAddTool(ctx, makeToolReq(map[string]any{
		"path": dest, "task_id": "MGIT-9.2",
	}))
	require.NoError(t, err)
	require.False(t, res.IsError, "add errored: %s", resultText(t, res))
	assert.NotContains(t, resultText(t, res), "not yet available")
	assert.Contains(t, resultText(t, res), "MGIT-9.2")

	// Real materialization: the worktree directory exists on disk.
	info, statErr := os.Stat(dest)
	require.NoError(t, statErr, "worktree dir was not materialized")
	assert.True(t, info.IsDir())

	// And it is registered — visible via the list tool.
	listRes, err := srv.worktreeListTool(ctx, makeToolReq(map[string]any{}))
	require.NoError(t, err)
	require.False(t, listRes.IsError)
	assert.Contains(t, resultText(t, listRes), "MGIT-9.2")
	assert.Contains(t, resultText(t, listRes), dest)
}

// TestMCP_WorktreeAddTool_InvalidInput covers the error paths: missing/empty
// required args and a malformed task id are rejected as tool errors, never a
// fake-success placeholder. Refs: MGIT-45
func TestMCP_WorktreeAddTool_InvalidInput(t *testing.T) {
	srv := setupTestMCP(t)
	dummy := filepath.Join(t.TempDir(), "x")

	tests := []struct {
		name string
		args map[string]any
	}{
		{"missing_path", map[string]any{"task_id": "MGIT-1.1"}},
		{"empty_path", map[string]any{"path": "", "task_id": "MGIT-1.1"}},
		{"missing_task_id", map[string]any{"path": dummy}},
		{"empty_task_id", map[string]any{"path": dummy, "task_id": ""}},
		{"malformed_task_id", map[string]any{"path": dummy, "task_id": "not a task!!"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := srv.worktreeAddTool(context.Background(), makeToolReq(tt.args))
			require.NoError(t, err)
			assert.True(t, res.IsError, "expected a tool error for %s", tt.name)
			assert.NotContains(t, resultText(t, res), "not yet available")
		})
	}
}

// TestMCP_WorktreeAddTool_DuplicateTaskRejected proves the append-only/isolation
// guarantees flow through: a second worktree bound to the same task fails.
// Refs: MGIT-45, FR-16
func TestMCP_WorktreeAddTool_DuplicateTaskRejected(t *testing.T) {
	srv := setupTestMCP(t)
	ctx := context.Background()
	_, err := srv.commitTool(ctx, makeToolReq(map[string]any{"task_id": "MGIT-9.1", "message": "base"}))
	require.NoError(t, err)

	first := filepath.Join(t.TempDir(), "a")
	res, err := srv.worktreeAddTool(ctx, makeToolReq(map[string]any{"path": first, "task_id": "MGIT-9.5"}))
	require.NoError(t, err)
	require.False(t, res.IsError, resultText(t, res))

	second := filepath.Join(t.TempDir(), "b")
	res2, err := srv.worktreeAddTool(ctx, makeToolReq(map[string]any{"path": second, "task_id": "MGIT-9.5"}))
	require.NoError(t, err)
	assert.True(t, res2.IsError, "duplicate task binding must be rejected")
}

// TestMCP_WorktreeListTool_Empty: a fresh server lists no worktrees without
// error and without placeholder text. Refs: MGIT-45
func TestMCP_WorktreeListTool_Empty(t *testing.T) {
	srv := setupTestMCP(t)
	res, err := srv.worktreeListTool(context.Background(), makeToolReq(map[string]any{}))
	require.NoError(t, err)
	require.False(t, res.IsError)
	assert.NotContains(t, resultText(t, res), "not yet available")
}

// TestMCP_WorktreeListTool_ServiceError proves a service-layer failure surfaces
// as a structured tool error (not a crash, not fake success): the underlying
// index is closed, so the list read fails. Refs: MGIT-45
func TestMCP_WorktreeListTool_ServiceError(t *testing.T) {
	tmpDir := t.TempDir()
	clock := fixedClock()
	repo, err := gitstore.Init(tmpDir, clock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = repo.Close() })
	idx, err := index.New(filepath.Join(tmpDir, ".mgit", "index.db"), clock)
	require.NoError(t, err)

	srv := NewServer(repo, idx)
	require.NoError(t, idx.Close()) // subsequent index reads now fail

	res, err := srv.worktreeListTool(context.Background(), makeToolReq(map[string]any{}))
	require.NoError(t, err)
	assert.True(t, res.IsError, "list on a closed index must surface a tool error")
}

// TestMCP_WorktreeRemoveTool covers the happy path (removes the registration)
// and the missing-path error path. Refs: MGIT-45
func TestMCP_WorktreeRemoveTool(t *testing.T) {
	srv := setupTestMCP(t)
	ctx := context.Background()
	_, err := srv.commitTool(ctx, makeToolReq(map[string]any{"task_id": "MGIT-9.1", "message": "base"}))
	require.NoError(t, err)

	dest := filepath.Join(t.TempDir(), "wt-remove")
	res, err := srv.worktreeAddTool(ctx, makeToolReq(map[string]any{"path": dest, "task_id": "MGIT-9.3"}))
	require.NoError(t, err)
	require.False(t, res.IsError, resultText(t, res))

	rmRes, err := srv.worktreeRemoveTool(ctx, makeToolReq(map[string]any{"path": dest}))
	require.NoError(t, err)
	require.False(t, rmRes.IsError, "remove errored: %s", resultText(t, rmRes))
	assert.NotContains(t, resultText(t, rmRes), "not yet available")

	listRes, err := srv.worktreeListTool(ctx, makeToolReq(map[string]any{}))
	require.NoError(t, err)
	assert.NotContains(t, resultText(t, listRes), "MGIT-9.3", "removed worktree still listed")

	t.Run("missing_path", func(t *testing.T) {
		res, err := srv.worktreeRemoveTool(ctx, makeToolReq(map[string]any{}))
		require.NoError(t, err)
		assert.True(t, res.IsError)
	})

	t.Run("unknown_path_errors", func(t *testing.T) {
		res, err := srv.worktreeRemoveTool(ctx, makeToolReq(map[string]any{
			"path": filepath.Join(t.TempDir(), "never-registered"),
		}))
		require.NoError(t, err)
		assert.True(t, res.IsError, "removing an unregistered worktree must error, not fake success")
	})
}

// TestMCP_WorktreeTools_NoPlaceholderText is the anti-stub guard for MGIT-45:
// none of the three worktree tools may return the old "not yet available"
// placeholder. Refs: MGIT-45, CLAUDE.md Rule 11
func TestMCP_WorktreeTools_NoPlaceholderText(t *testing.T) {
	srv := setupTestMCP(t)
	ctx := context.Background()
	dummy := filepath.Join(t.TempDir(), "x")

	handlers := map[string]func() *mcptypes.CallToolResult{
		"add": func() *mcptypes.CallToolResult {
			r, _ := srv.worktreeAddTool(ctx, makeToolReq(map[string]any{"path": dummy, "task_id": "MGIT-1.1"}))
			return r
		},
		"list": func() *mcptypes.CallToolResult {
			r, _ := srv.worktreeListTool(ctx, makeToolReq(map[string]any{}))
			return r
		},
		"remove": func() *mcptypes.CallToolResult {
			r, _ := srv.worktreeRemoveTool(ctx, makeToolReq(map[string]any{"path": dummy}))
			return r
		},
	}
	for name, h := range handlers {
		t.Run(name, func(t *testing.T) {
			assert.NotContains(t, resultText(t, h()), "not yet available",
				"tool %s still returns placeholder text", name)
		})
	}
}
