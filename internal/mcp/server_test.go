package mcp

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	mcptypes "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gitstore "github.com/hyper-swe/mgit/internal/store/git"
	"github.com/hyper-swe/mgit/internal/store/index"
)

func fixedClock() func() time.Time {
	return func() time.Time { return time.Date(2026, 4, 7, 12, 0, 0, 0, time.UTC) }
}

func setupTestMCP(t *testing.T, opts ...Option) *Server {
	t.Helper()
	srv, _ := setupTestMCPWithRepo(t, opts...)
	return srv
}

// setupTestMCPWith builds a test server with extra options wired (e.g. the
// sandbox connector). Refs: MGIT-76
func setupTestMCPWith(t *testing.T, opts ...Option) *Server {
	t.Helper()
	srv, _ := setupTestMCPWithRepo(t, opts...)
	return srv
}

// setupTestMCPWithRepo also returns the backing repository, for tests that
// need to stage real working files (content-bearing commits, MGIT-54).
func setupTestMCPWithRepo(t *testing.T, opts ...Option) (*Server, *gitstore.Repository) {
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

	return NewServer(repo, idx, opts...), repo
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

// TestMCP_CommitTool pins the MCP surface's staging contract: this server
// exposes no staging tool, so mgit_commit stages the working tree by default
// and the resulting commit must actually carry the file. Without that default
// every MCP commit would record an empty tree (MGIT-77). Refs: FR-10, MGIT-77
func TestMCP_CommitTool(t *testing.T) {
	srv, repo := setupTestMCPWithRepo(t)
	ctx := context.Background()

	require.NoError(t, os.WriteFile(filepath.Join(repo.Root(), "via-mcp.txt"),
		[]byte("committed through MCP\n"), 0o600))

	result, err := srv.commitTool(ctx, makeToolReq(map[string]any{
		"task_id": "MGIT-1.2.3",
		"message": "test commit via MCP",
	}))
	require.NoError(t, err)
	require.False(t, result.IsError, resultText(t, result))

	commits, err := srv.commit.ListCommits(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, commits)
	content, err := gitstore.NewCommitStore(repo).GetFileFromCommit(ctx, commits[0].CommitID, "via-mcp.txt")
	require.NoError(t, err, "the MCP commit must contain the working-tree file")
	assert.Equal(t, "committed through MCP\n", string(content))
}

// An MCP commit that would record nothing is refused, like every other surface.
// Refs: MGIT-77
func TestMCP_CommitTool_NothingToCommit_ReturnsError(t *testing.T) {
	srv := setupTestMCP(t)

	result, err := srv.commitTool(context.Background(), makeToolReq(map[string]any{
		"task_id": "MGIT-1.2.3",
		"message": "records nothing",
	}))
	require.NoError(t, err)
	require.True(t, result.IsError, "an empty commit must not report success")
	assert.Contains(t, resultText(t, result), "nothing to commit")
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
		"task_id": "MGIT-3.1", "message": "pre-squash", "allow_empty": true,
	}))
	require.NoError(t, err)

	result, err := srv.squashTool(ctx, makeToolReq(map[string]any{
		"task_id": "MGIT-3.1",
	}))
	require.NoError(t, err)
	assert.False(t, result.IsError)
}

func TestMCP_RollbackTool(t *testing.T) {
	srv, repo := setupTestMCPWithRepo(t)
	ctx := context.Background()

	// Rollback reverts REAL tree changes (MGIT-54): stage a file first.
	require.NoError(t, os.WriteFile(filepath.Join(repo.Root(), "rb.txt"), []byte("v1\n"), 0o600))
	require.NoError(t, gitstore.NewWorktreeStore(repo).Add(ctx, "rb.txt"))

	_, err := srv.commitTool(ctx, makeToolReq(map[string]any{
		"task_id": "MGIT-4.1", "message": "to rollback", "allow_empty": true,
	}))
	require.NoError(t, err)

	result, err := srv.rollbackTool(ctx, makeToolReq(map[string]any{
		"task_id": "MGIT-4.1", "reason": "test",
	}))
	require.NoError(t, err)
	assert.False(t, result.IsError)
}
