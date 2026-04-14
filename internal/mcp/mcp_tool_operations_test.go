package mcp

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMCP_StatusTool(t *testing.T) {
	srv := setupTestMCP(t)
	result, err := srv.statusTool(context.Background(), makeToolReq(map[string]any{}))
	require.NoError(t, err)
	assert.False(t, result.IsError)
}

func TestMCP_ShowTool(t *testing.T) {
	srv := setupTestMCP(t)
	ctx := context.Background()

	// Create a commit first
	commitResult, err := srv.commitTool(ctx, makeToolReq(map[string]any{
		"task_id": "MGIT-1.1", "message": "for show",
	}))
	require.NoError(t, err)
	assert.False(t, commitResult.IsError)

	// Show with invalid hash returns error content
	result, err := srv.showTool(ctx, makeToolReq(map[string]any{
		"commit_id": "0000000000000000000000000000000000000000",
	}))
	require.NoError(t, err)
	assert.True(t, result.IsError)
}

func TestMCP_DiffTool(t *testing.T) {
	srv := setupTestMCP(t)
	result, err := srv.diffTool(context.Background(), makeToolReq(map[string]any{}))
	require.NoError(t, err)
	assert.False(t, result.IsError)
}

func TestMCP_ExportTool(t *testing.T) {
	srv := setupTestMCP(t)
	ctx := context.Background()

	_, err := srv.commitTool(ctx, makeToolReq(map[string]any{
		"task_id": "MGIT-2.1", "message": "for export",
	}))
	require.NoError(t, err)

	result, err := srv.exportTool(ctx, makeToolReq(map[string]any{
		"task_id": "MGIT-2.1",
	}))
	require.NoError(t, err)
	assert.False(t, result.IsError)
}

func TestMCP_ExportTool_EmptyTask(t *testing.T) {
	srv := setupTestMCP(t)
	result, err := srv.exportTool(context.Background(), makeToolReq(map[string]any{
		"task_id": "MGIT-99.9",
	}))
	require.NoError(t, err)
	assert.False(t, result.IsError) // empty list, not error
}

func TestMCP_AuditTool(t *testing.T) {
	srv := setupTestMCP(t)
	result, err := srv.auditTool(context.Background(), makeToolReq(map[string]any{}))
	require.NoError(t, err)
	assert.False(t, result.IsError)
}

func TestMCP_ConfigTool(t *testing.T) {
	srv := setupTestMCP(t)
	result, err := srv.configTool(context.Background(), makeToolReq(map[string]any{}))
	require.NoError(t, err)
	assert.False(t, result.IsError)
}

func TestMCP_WorktreeAddTool(t *testing.T) {
	srv := setupTestMCP(t)
	result, err := srv.worktreeAddTool(context.Background(), makeToolReq(map[string]any{
		"path": "/tmp/wt", "task_id": "MGIT-1.1",
	}))
	require.NoError(t, err)
	assert.False(t, result.IsError)
}

func TestMCP_WorktreeListTool(t *testing.T) {
	srv := setupTestMCP(t)
	result, err := srv.worktreeListTool(context.Background(), makeToolReq(map[string]any{}))
	require.NoError(t, err)
	assert.False(t, result.IsError)
}

func TestMCP_WorktreeRemoveTool(t *testing.T) {
	srv := setupTestMCP(t)
	result, err := srv.worktreeRemoveTool(context.Background(), makeToolReq(map[string]any{
		"path": "/tmp/wt",
	}))
	require.NoError(t, err)
	assert.False(t, result.IsError)
}

func TestMCP_LogTool_WithTaskID(t *testing.T) {
	srv := setupTestMCP(t)
	ctx := context.Background()

	_, err := srv.commitTool(ctx, makeToolReq(map[string]any{
		"task_id": "MGIT-3.1", "message": "for log",
	}))
	require.NoError(t, err)

	result, err := srv.logTool(ctx, makeToolReq(map[string]any{
		"task_id": "MGIT-3.1",
	}))
	require.NoError(t, err)
	assert.False(t, result.IsError)
}

func TestMCP_BranchTool_List(t *testing.T) {
	srv := setupTestMCP(t)
	result, err := srv.branchTool(context.Background(), makeToolReq(map[string]any{}))
	require.NoError(t, err)
	assert.False(t, result.IsError)
}

func TestMCP_VerifyTool_WithTaskID(t *testing.T) {
	srv := setupTestMCP(t)
	ctx := context.Background()

	_, err := srv.commitTool(ctx, makeToolReq(map[string]any{
		"task_id": "MGIT-4.1", "message": "for verify",
	}))
	require.NoError(t, err)

	result, err := srv.verifyTool(ctx, makeToolReq(map[string]any{
		"task_id": "MGIT-4.1",
	}))
	require.NoError(t, err)
	assert.False(t, result.IsError)
}

func TestMCP_CommitTool_DefaultAgentID(t *testing.T) {
	srv := setupTestMCP(t)
	result, err := srv.commitTool(context.Background(), makeToolReq(map[string]any{
		"task_id": "MGIT-5.1",
	}))
	require.NoError(t, err)
	assert.False(t, result.IsError)
}

func TestMCP_CommitTool_InvalidTaskID(t *testing.T) {
	srv := setupTestMCP(t)
	result, err := srv.commitTool(context.Background(), makeToolReq(map[string]any{
		"task_id": "invalid",
	}))
	require.NoError(t, err)
	assert.True(t, result.IsError)
}

func TestMCP_RollbackTool_EmptyTask(t *testing.T) {
	srv := setupTestMCP(t)
	result, err := srv.rollbackTool(context.Background(), makeToolReq(map[string]any{
		"task_id": "MGIT-99.9",
	}))
	require.NoError(t, err)
	assert.True(t, result.IsError)
}

func TestMCP_SquashTool_EmptyTask(t *testing.T) {
	srv := setupTestMCP(t)
	result, err := srv.squashTool(context.Background(), makeToolReq(map[string]any{
		"task_id": "MGIT-99.9",
	}))
	require.NoError(t, err)
	assert.True(t, result.IsError)
}
