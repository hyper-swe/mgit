package mcp

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	mcptypes "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gitstore "github.com/hyper-swe/mgit-dev/internal/store/git"
	"github.com/hyper-swe/mgit-dev/internal/store/index"
)

func fixedClock() func() time.Time {
	return func() time.Time { return time.Date(2026, 4, 7, 12, 0, 0, 0, time.UTC) }
}

func setupTestMCP(t *testing.T) *Server {
	t.Helper()
	tmpDir := t.TempDir()
	clock := fixedClock()

	repo, err := gitstore.Init(tmpDir, clock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = repo.Close() })

	dbPath := filepath.Join(tmpDir, ".mgit", "index.db")
	idx, err := index.New(dbPath, clock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = idx.Close() })

	return NewServer(repo, idx)
}

func makeToolReq(args map[string]any) mcptypes.CallToolRequest {
	return mcptypes.CallToolRequest{
		Params: mcptypes.CallToolParams{
			Arguments: args,
		},
	}
}

func TestMCP_Server_Init(t *testing.T) {
	srv := setupTestMCP(t)
	assert.NotNil(t, srv)
	assert.NotNil(t, srv.MCPServer())
}

func TestMCP_CommitTool(t *testing.T) {
	srv := setupTestMCP(t)
	ctx := context.Background()

	result, err := srv.commitTool(ctx, makeToolReq(map[string]any{
		"task_id": "MGIT-1.2.3",
		"message": "test commit via MCP",
	}))
	require.NoError(t, err)
	assert.False(t, result.IsError)
}

func TestMCP_LogTool(t *testing.T) {
	srv := setupTestMCP(t)
	ctx := context.Background()

	result, err := srv.logTool(ctx, makeToolReq(map[string]any{}))
	require.NoError(t, err)
	assert.False(t, result.IsError)
}

func TestMCP_BranchTool(t *testing.T) {
	srv := setupTestMCP(t)
	ctx := context.Background()

	result, err := srv.branchTool(ctx, makeToolReq(map[string]any{
		"task_id": "MGIT-2.1",
	}))
	require.NoError(t, err)
	assert.False(t, result.IsError)
}

func TestMCP_VerifyTool(t *testing.T) {
	srv := setupTestMCP(t)
	ctx := context.Background()

	result, err := srv.verifyTool(ctx, makeToolReq(map[string]any{}))
	require.NoError(t, err)
	assert.False(t, result.IsError)
}

func TestMCP_SquashTool(t *testing.T) {
	srv := setupTestMCP(t)
	ctx := context.Background()

	// First create a commit
	_, err := srv.commitTool(ctx, makeToolReq(map[string]any{
		"task_id": "MGIT-3.1", "message": "pre-squash",
	}))
	require.NoError(t, err)

	result, err := srv.squashTool(ctx, makeToolReq(map[string]any{
		"task_id": "MGIT-3.1",
	}))
	require.NoError(t, err)
	assert.False(t, result.IsError)
}

func TestMCP_RollbackTool(t *testing.T) {
	srv := setupTestMCP(t)
	ctx := context.Background()

	_, err := srv.commitTool(ctx, makeToolReq(map[string]any{
		"task_id": "MGIT-4.1", "message": "to rollback",
	}))
	require.NoError(t, err)

	result, err := srv.rollbackTool(ctx, makeToolReq(map[string]any{
		"task_id": "MGIT-4.1", "reason": "test",
	}))
	require.NoError(t, err)
	assert.False(t, result.IsError)
}
