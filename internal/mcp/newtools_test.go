package mcp

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	mcptypes "github.com/mark3labs/mcp-go/mcp"

	gitstore "github.com/hyper-swe/mgit/internal/store/git"
	"github.com/hyper-swe/mgit/internal/store/index"
)

type mcpResultT = mcptypes.CallToolResult

// TestMCP_ContentBackedTools exercises the tools that need real committed
// content: diff for a task (a materialized unified diff), status with a change,
// and config set+save. Refs: MGIT-50
func TestMCP_ContentBackedTools(t *testing.T) {
	tmpDir := t.TempDir()
	clock := fixedClock()
	repo, err := gitstore.Init(tmpDir, clock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = repo.Close() })
	idx, err := index.New(filepath.Join(tmpDir, ".mgit", "index.db"), clock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = idx.Close() })
	srv := NewServer(repo, idx)
	ctx := context.Background()

	// A content commit on a task.
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "f.go"), []byte("package f\n\nfunc F() int { return 1 }\n"), 0o600))
	require.NoError(t, srv.wtStore.Add(ctx, "."))
	_, err = srv.commitTool(ctx, makeToolReq(map[string]any{"task_id": "MGIT-9.1", "message": "add f"}))
	require.NoError(t, err)

	// diff for the task carries the real added file.
	d, err := srv.diffTool(ctx, makeToolReq(map[string]any{"task_id": "MGIT-9.1"}))
	require.NoError(t, err)
	require.False(t, d.IsError, resultText(t, d))
	assert.Contains(t, resultText(t, d), "f.go")

	// status reflects a new working-tree change.
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "g.go"), []byte("package g\n"), 0o600))
	s, err := srv.statusTool(ctx, makeToolReq(map[string]any{}))
	require.NoError(t, err)
	require.False(t, s.IsError, resultText(t, s))

	// config set persists (Save path).
	c, err := srv.configTool(ctx, makeToolReq(map[string]any{"key": "project.name", "value": "demo"}))
	require.NoError(t, err)
	assert.False(t, c.IsError, resultText(t, c))
}

// TestMCP_ServiceErrors_Structured proves service-layer failures surface as
// structured tool errors (not crashes): with the index closed, the index-backed
// tools all return IsError. Refs: MGIT-50
func TestMCP_ServiceErrors_Structured(t *testing.T) {
	tmpDir := t.TempDir()
	clock := fixedClock()
	repo, err := gitstore.Init(tmpDir, clock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = repo.Close() })
	idx, err := index.New(filepath.Join(tmpDir, ".mgit", "index.db"), clock)
	require.NoError(t, err)

	srv := NewServer(repo, idx)
	require.NoError(t, idx.Close()) // index reads/writes now fail

	ctx := context.Background()
	mustErr := func(name string, res *mcpResultT, err error) {
		t.Helper()
		require.NoError(t, err)
		assert.True(t, res.IsError, "%s must surface the index failure as a tool error", name)
	}
	r, e := srv.commitTool(ctx, makeToolReq(map[string]any{"task_id": "MGIT-1.1", "message": "x"}))
	mustErr("commit", r, e)
	r, e = srv.logTool(ctx, makeToolReq(map[string]any{}))
	mustErr("log", r, e)
	r, e = srv.exportTool(ctx, makeToolReq(map[string]any{"task_id": "MGIT-1.1"}))
	mustErr("export", r, e)
	r, e = srv.showTool(ctx, makeToolReq(map[string]any{"commit_id": "0000000000000000000000000000000000000000"}))
	mustErr("show", r, e)
}

// TestMCP_ConfigTool_GetSetAll covers the real config tool: get the whole
// config, get a known key, and set+get a known key round-trip. Refs: MGIT-50
func TestMCP_ConfigTool_GetSetAll(t *testing.T) {
	srv := setupTestMCP(t)
	ctx := context.Background()

	all, err := srv.configTool(ctx, makeToolReq(map[string]any{}))
	require.NoError(t, err)
	require.False(t, all.IsError, resultText(t, all))
	assert.Contains(t, resultText(t, all), "project", "getAll returns the config JSON")

	got, err := srv.configTool(ctx, makeToolReq(map[string]any{"key": "project"}))
	require.NoError(t, err)
	assert.False(t, got.IsError, "get a known key succeeds")

	set, err := srv.configTool(ctx, makeToolReq(map[string]any{"key": "project.name", "value": "demo"}))
	require.NoError(t, err)
	require.False(t, set.IsError, resultText(t, set))
	assert.Contains(t, resultText(t, set), "demo")

	back, err := srv.configTool(ctx, makeToolReq(map[string]any{"key": "project.name"}))
	require.NoError(t, err)
	require.False(t, back.IsError)
	assert.Contains(t, resultText(t, back), "demo", "set value round-trips")

	miss, err := srv.configTool(ctx, makeToolReq(map[string]any{"key": "nope.notthere"}))
	require.NoError(t, err)
	assert.True(t, miss.IsError, "unknown key is a structured error")
}

// TestMCP_AuditTool_ReturnsEntries covers the real audit tool: after a commit
// the append-only trail carries an entry, unfiltered and filtered by task.
// Refs: MGIT-50, MGIT-20
func TestMCP_AuditTool_ReturnsEntries(t *testing.T) {
	srv := setupTestMCP(t)
	ctx := context.Background()
	_, err := srv.commitTool(ctx, makeToolReq(map[string]any{"task_id": "MGIT-7.1", "message": "step"}))
	require.NoError(t, err)

	all, err := srv.auditTool(ctx, makeToolReq(map[string]any{}))
	require.NoError(t, err)
	require.False(t, all.IsError, resultText(t, all))
	assert.Contains(t, resultText(t, all), "MGIT-7.1", "audit trail carries the commit's task")

	filtered, err := srv.auditTool(ctx, makeToolReq(map[string]any{"task_id": "MGIT-7.1"}))
	require.NoError(t, err)
	assert.False(t, filtered.IsError)
}

// TestMCP_StatusTool_ReturnsJSON covers the real status tool on a clean repo:
// it syncs then returns the file-status JSON (not the old placeholder).
// Refs: MGIT-50
func TestMCP_StatusTool_ReturnsJSON(t *testing.T) {
	srv := setupTestMCP(t)
	res, err := srv.statusTool(context.Background(), makeToolReq(map[string]any{}))
	require.NoError(t, err)
	require.False(t, res.IsError, resultText(t, res))
	assert.NotContains(t, resultText(t, res), "working tree clean")
}

// TestMCP_DiffTool_CommitPair covers the diff tool's commit-pair path: two real
// commits produce a (possibly empty) diff without error. Refs: MGIT-50
func TestMCP_DiffTool_CommitPair(t *testing.T) {
	srv := setupTestMCP(t)
	ctx := context.Background()
	_, err := srv.commitTool(ctx, makeToolReq(map[string]any{"task_id": "MGIT-7.1", "message": "one"}))
	require.NoError(t, err)
	_, err = srv.commitTool(ctx, makeToolReq(map[string]any{"task_id": "MGIT-7.2", "message": "two"}))
	require.NoError(t, err)

	commits, err := srv.commit.ListCommits(ctx)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(commits), 2)

	res, err := srv.diffTool(ctx, makeToolReq(map[string]any{
		"commit1": commits[1].CommitID, "commit2": commits[0].CommitID,
	}))
	require.NoError(t, err)
	assert.False(t, res.IsError, "diff between two real commits succeeds: %s", resultText(t, res))
}

// TestMCP_SquashRollbackBranch_SuccessPaths covers the write tools' success
// paths end to end: commit → squash → rollback, and branch create. Refs: MGIT-50
func TestMCP_SquashRollbackBranch_SuccessPaths(t *testing.T) {
	srv, repo := setupTestMCPWithRepo(t)
	ctx := context.Background()

	// Rollback reverts REAL tree changes (MGIT-54): stage a file first.
	require.NoError(t, os.WriteFile(filepath.Join(repo.Root(), "sq.txt"), []byte("v1\n"), 0o600))
	require.NoError(t, gitstore.NewWorktreeStore(repo).Add(ctx, "sq.txt"))

	_, err := srv.commitTool(ctx, makeToolReq(map[string]any{"task_id": "MGIT-8.1", "message": "s1"}))
	require.NoError(t, err)

	sq, err := srv.squashTool(ctx, makeToolReq(map[string]any{"task_id": "MGIT-8.1", "message": "squashed"}))
	require.NoError(t, err)
	assert.False(t, sq.IsError, resultText(t, sq))

	rb, err := srv.rollbackTool(ctx, makeToolReq(map[string]any{"task_id": "MGIT-8.1", "reason": "undo"}))
	require.NoError(t, err)
	assert.False(t, rb.IsError, resultText(t, rb))

	br, err := srv.branchTool(ctx, makeToolReq(map[string]any{"task_id": "MGIT-8.2"}))
	require.NoError(t, err)
	assert.False(t, br.IsError, resultText(t, br))
	assert.Contains(t, resultText(t, br), "branch")
}

// TestMCP_VerifyTool_TaskPath covers the verify tool's task-scoped branch (the
// no-arg index path is covered elsewhere). Refs: MGIT-50
func TestMCP_VerifyTool_TaskPath(t *testing.T) {
	srv := setupTestMCP(t)
	ctx := context.Background()
	_, err := srv.commitTool(ctx, makeToolReq(map[string]any{"task_id": "MGIT-7.1", "message": "s"}))
	require.NoError(t, err)

	res, err := srv.verifyTool(ctx, makeToolReq(map[string]any{"task_id": "MGIT-7.1"}))
	require.NoError(t, err)
	assert.False(t, res.IsError, resultText(t, res))
	assert.Contains(t, resultText(t, res), "verified")
}
