package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gitstore "github.com/hyper-swe/mgit/internal/store/git"
)

// --- AuditService error / edge paths ---

// TestAuditService_LogOperation_OpenError_MissingDir: appending to a log under a
// non-existent directory surfaces the open error (O_CREATE does not mkdir).
func TestAuditService_LogOperation_OpenError_MissingDir(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "no-such-dir", "audit.log")
	svc := NewAuditService(logPath, fixedClock())
	err := svc.LogOperation(AuditEntry{Operation: AuditCreateCommit, TaskID: "MGIT-1"})
	assert.Error(t, err, "logging under a missing directory must fail to open the file")
}

// TestAuditService_GetAuditLog_SkipsMalformedLines: a corrupt JSON line in the
// log is skipped, valid entries still returned.
func TestAuditService_GetAuditLog_SkipsMalformedLines(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "audit.log")
	svc := NewAuditService(logPath, fixedClock())
	require.NoError(t, svc.LogOperation(AuditEntry{Operation: AuditCreateCommit, TaskID: "MGIT-1", AgentID: "a"}))
	// Append a malformed line directly.
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // test-controlled path under t.TempDir
	require.NoError(t, err)
	_, _ = f.WriteString("{ this is not valid json\n")
	require.NoError(t, f.Close())
	require.NoError(t, svc.LogOperation(AuditEntry{Operation: AuditSquash, TaskID: "MGIT-2", AgentID: "b"}))

	entries, err := svc.GetAuditLog(AuditFilters{})
	require.NoError(t, err)
	assert.Len(t, entries, 2, "the two valid entries are returned; the malformed line is skipped")
}

// TestAuditService_GetAuditLog_FilterEachField: task, agent, and operation
// filters each select the matching subset (both match and non-match outcomes).
func TestAuditService_GetAuditLog_FilterEachField(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "audit.log")
	svc := NewAuditService(logPath, fixedClock())
	require.NoError(t, svc.LogOperation(AuditEntry{Operation: AuditCreateCommit, TaskID: "MGIT-1", AgentID: "alice"}))
	require.NoError(t, svc.LogOperation(AuditEntry{Operation: AuditSquash, TaskID: "MGIT-2", AgentID: "bob"}))

	byTask, err := svc.GetAuditLog(AuditFilters{TaskID: "MGIT-1"})
	require.NoError(t, err)
	assert.Len(t, byTask, 1)
	byAgent, err := svc.GetAuditLog(AuditFilters{AgentID: "bob"})
	require.NoError(t, err)
	assert.Len(t, byAgent, 1)
	byOp, err := svc.GetAuditLog(AuditFilters{Operation: AuditSquash})
	require.NoError(t, err)
	assert.Len(t, byOp, 1)
	none, err := svc.GetAuditLog(AuditFilters{TaskID: "MGIT-9"})
	require.NoError(t, err)
	assert.Empty(t, none)
}

// TestAuditService_ExportAuditLog_ReadError: when the log path is unreadable (a
// directory), Export propagates the read error rather than returning JSON.
func TestAuditService_ExportAuditLog_ReadError(t *testing.T) {
	dirAsPath := t.TempDir() // a directory, not a file
	svc := NewAuditService(dirAsPath, fixedClock())
	_, err := svc.ExportAuditLog(AuditFilters{})
	assert.Error(t, err, "reading a directory as the audit log must error")
}

// --- DiffService error paths ---

// TestDiffService_DiffCommits_BadHashes_Error: diffing non-existent commits errors.
func TestDiffService_DiffCommits_BadHashes_Error(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()
	ds := NewDiffService(gitstore.NewDiffStore(env.repo), env.cs, env.idx)
	_, err := ds.DiffCommits(ctx, "0123456789abcdef0123456789abcdef01234567",
		"89abcdef0123456789abcdef0123456789abcdef")
	assert.Error(t, err, "diffing missing commits must error")
}

// TestDiffService_DiffTask_BadRecord_Error: a task record pointing at a missing
// commit fails the task diff at the first-commit lookup.
func TestDiffService_DiffTask_BadRecord_Error(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()
	ds := NewDiffService(gitstore.NewDiffStore(env.repo), env.cs, env.idx)
	require.NoError(t, env.idx.AddCommitToTask(ctx, "MGIT-3.9",
		"0123456789abcdef0123456789abcdef01234567", "deadbeef", "agent", 0))
	_, err := ds.DiffTask(ctx, "MGIT-3.9")
	assert.Error(t, err, "a task whose first commit is missing must error")
}

// --- BranchService error paths ---

// TestBranchService_CreateNamedBranch_Duplicate_Error: creating the same named
// branch twice fails on the second create.
func TestBranchService_CreateNamedBranch_Duplicate_Error(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()
	_, err := env.branch.CreateNamedBranch(ctx, "feature-x")
	require.NoError(t, err)
	_, err = env.branch.CreateNamedBranch(ctx, "feature-x")
	assert.Error(t, err, "a duplicate named branch must be rejected")
}

// --- RollbackService error paths ---

// TestRollbackService_RollbackTask_BadRecord_Error: a task record pointing at a
// missing commit fails the rollback at the diff-collection step.
func TestRollbackService_RollbackTask_BadRecord_Error(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()
	require.NoError(t, env.idx.AddCommitToTask(ctx, "MGIT-6.9",
		"0123456789abcdef0123456789abcdef01234567", "deadbeef", "agent", 0))
	_, err := env.rollbk.RollbackTask(ctx, RollbackRequest{TaskID: "MGIT-6.9"})
	assert.Error(t, err, "rollback over a missing commit must error")
}
