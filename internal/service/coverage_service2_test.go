package service

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/astutic/mgit/internal/model"
)

// --- SquashService: mergeDiffs with duplicate paths ---

func TestSquashService_MergeDiffs_DuplicatePaths(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	// Two commits modifying the same file
	_, err := env.commit.CreateCommit(ctx, CreateCommitRequest{
		TaskID: "MGIT-7.1", AgentID: "a",
		FileDiffs: []model.FileDiff{
			{Path: "main.go", Operation: model.DiffModified, OldHash: "a", NewHash: "b"},
		},
	})
	require.NoError(t, err)

	_, err = env.commit.CreateCommit(ctx, CreateCommitRequest{
		TaskID: "MGIT-7.1", AgentID: "a",
		FileDiffs: []model.FileDiff{
			{Path: "main.go", Operation: model.DiffModified, OldHash: "b", NewHash: "c"},
		},
	})
	require.NoError(t, err)

	squashed, err := env.squash.SquashTask(ctx, SquashRequest{TaskID: "MGIT-7.1"})
	require.NoError(t, err)
	assert.Equal(t, model.CommitTypeSquash, squashed.CommitType)
}

// --- ConfigService: save and reload ---

func TestConfigService_SaveAndReload(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	svc, err := NewConfigService(configPath)
	require.NoError(t, err)

	require.NoError(t, svc.Set("logging.level", "debug"))
	require.NoError(t, svc.Save())

	svc2, err := NewConfigService(configPath)
	require.NoError(t, err)

	val, err := svc2.Get("logging.level")
	require.NoError(t, err)
	assert.Equal(t, "debug", val)
}

// --- ConfigService: set deep nested key ---

func TestConfigService_Set_DeepNestedKey(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	svc, err := NewConfigService(configPath)
	require.NoError(t, err)

	require.NoError(t, svc.Set("audit.log_file", "/custom/audit.log"))
	val, err := svc.Get("audit.log_file")
	require.NoError(t, err)
	assert.Equal(t, "/custom/audit.log", val)
}

// --- AuditService: log and filter by operation ---

func TestAuditService_LogAndFilterByOperation(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "audit.log")
	svc := NewAuditService(logPath, fixedClock())

	require.NoError(t, svc.LogOperation(AuditEntry{
		Operation: AuditCreateCommit, AgentID: "a1", TaskID: "MGIT-1.1",
	}))
	require.NoError(t, svc.LogOperation(AuditEntry{
		Operation: AuditRollback, AgentID: "a1", TaskID: "MGIT-1.1",
	}))

	entries, err := svc.GetAuditLog(AuditFilters{Operation: AuditRollback})
	require.NoError(t, err)
	assert.Len(t, entries, 1)
	assert.Equal(t, AuditRollback, entries[0].Operation)
}

// --- DiffService: statistics with zero hunks ---

func TestDiffService_Statistics_EmptyDiffs(t *testing.T) {
	ds := &DiffService{}
	stats := ds.Statistics(nil)
	assert.Equal(t, 0, stats.LinesAdded)
	assert.Equal(t, 0, stats.LinesRemoved)
}

// --- VerifyService: chain with single commit ---

func TestVerifyService_VerifyCommitChain_Single(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	c, err := env.commit.CreateCommit(ctx, CreateCommitRequest{
		TaskID: "MGIT-8.1", AgentID: "a", Message: "single",
	})
	require.NoError(t, err)

	verifySvc := NewVerifyService(env.cs, env.idx)
	err = verifySvc.VerifyCommitChain(ctx, []string{c.CommitID})
	assert.NoError(t, err)
}

// --- WorktreeService: add with existing branch ---

func TestWorktreeService_Add_ExistingBranch(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	// Create branch first
	_, err := env.branch.CreateBranch(ctx, "MGIT-9.1")
	require.NoError(t, err)

	wtSvc := NewWorktreeService(env.idx, env.branch, fixedClock())
	wt, err := wtSvc.Add(ctx, model.WorktreeAddOptions{
		Path: "/tmp/wt-existing", TaskID: "MGIT-9.1",
	})
	require.NoError(t, err)
	assert.Equal(t, "task/MGIT-9.1", wt.Branch)
}

// --- BranchService: create duplicate ---

func TestBranchService_CreateBranch_Duplicate(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	_, err := env.branch.CreateBranch(ctx, "MGIT-9.2")
	require.NoError(t, err)

	_, err = env.branch.CreateBranch(ctx, "MGIT-9.2")
	assert.Error(t, err)
}

// --- BranchService: invalid task ID ---

func TestBranchService_CreateBranch_InvalidTaskID(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	_, err := env.branch.CreateBranch(ctx, "invalid")
	assert.Error(t, err)
}
