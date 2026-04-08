package service

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/astutic/mgit/internal/model"
	gitstore "github.com/astutic/mgit/internal/store/git"
)

// --- CommitService gaps ---

func TestCommitService_GetCommit(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	c, err := env.commit.CreateCommit(ctx, CreateCommitRequest{
		TaskID: "MGIT-1.1", AgentID: "a", Message: "test",
	})
	require.NoError(t, err)

	got, err := env.commit.GetCommit(ctx, c.CommitID)
	require.NoError(t, err)
	assert.Equal(t, c.CommitID, got.CommitID)
}

func TestCommitService_ListCommits(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	_, err := env.commit.CreateCommit(ctx, CreateCommitRequest{
		TaskID: "MGIT-1.1", AgentID: "a", Message: "one",
	})
	require.NoError(t, err)

	commits, err := env.commit.ListCommits(ctx)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(commits), 1)
}

func TestCommitService_AutoMessage_Empty(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	c, err := env.commit.CreateCommit(ctx, CreateCommitRequest{
		TaskID: "MGIT-1.2", AgentID: "a",
	})
	require.NoError(t, err)
	assert.Contains(t, c.Message, "empty commit")
}

func TestCommitService_AutoMessage_MultipleDiffs(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	c, err := env.commit.CreateCommit(ctx, CreateCommitRequest{
		TaskID: "MGIT-1.3", AgentID: "a",
		FileDiffs: []model.FileDiff{
			{Path: "a.go", Operation: model.DiffAdded},
			{Path: "b.go", Operation: model.DiffAdded},
			{Path: "c.go", Operation: model.DiffAdded},
		},
	})
	require.NoError(t, err)
	assert.Contains(t, c.Message, "3 files changed")
}

// --- DiffService gaps ---

func TestDiffService_DiffRange(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	head1, err := env.repo.Head()
	require.NoError(t, err)

	_, err = env.commit.CreateCommit(ctx, CreateCommitRequest{
		TaskID: "MGIT-2.1", AgentID: "a", Message: "change",
	})
	require.NoError(t, err)
	head2, err := env.repo.Head()
	require.NoError(t, err)

	ds := gitstore.NewDiffStore(env.repo)
	diffSvc := NewDiffService(ds, env.cs, env.idx)

	diffs, err := diffSvc.DiffRange(ctx, head1, head2)
	require.NoError(t, err)
	assert.NotNil(t, diffs)
}

// --- BranchService gaps ---

func TestBranchService_GetBranch(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	b, err := env.branch.GetBranch(ctx, "main")
	require.NoError(t, err)
	assert.Equal(t, "main", b.Name)
}

// --- WorktreeService gaps ---

func TestWorktreeService_Add(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()
	clock := fixedClock()

	wtSvc := NewWorktreeService(env.idx, env.branch, clock)

	wt, err := wtSvc.Add(ctx, model.WorktreeAddOptions{
		Path: "/tmp/test-wt", TaskID: "MGIT-8.1", AgentID: "agent-01",
	})
	require.NoError(t, err)
	assert.Equal(t, "MGIT-8.1", wt.TaskID)
	assert.Equal(t, "task/MGIT-8.1", wt.Branch)
}

func TestWorktreeService_List(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()
	clock := fixedClock()

	wtSvc := NewWorktreeService(env.idx, env.branch, clock)

	wts, err := wtSvc.List(ctx)
	require.NoError(t, err)
	assert.Empty(t, wts)
}

func TestWorktreeService_AddAndList(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()
	clock := fixedClock()

	wtSvc := NewWorktreeService(env.idx, env.branch, clock)

	_, err := wtSvc.Add(ctx, model.WorktreeAddOptions{
		Path: "/tmp/wt1", TaskID: "MGIT-8.2", AgentID: "a1",
	})
	require.NoError(t, err)

	wts, err := wtSvc.List(ctx)
	require.NoError(t, err)
	assert.Len(t, wts, 1)
}

func TestWorktreeService_Remove(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()
	clock := fixedClock()

	wtSvc := NewWorktreeService(env.idx, env.branch, clock)

	_, err := wtSvc.Add(ctx, model.WorktreeAddOptions{
		Path: "/tmp/wt-rm", TaskID: "MGIT-8.3", AgentID: "a1",
	})
	require.NoError(t, err)

	err = wtSvc.Remove(ctx, "/tmp/wt-rm", false)
	assert.NoError(t, err)
}

func TestWorktreeService_Resolve(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()
	clock := fixedClock()

	wtSvc := NewWorktreeService(env.idx, env.branch, clock)

	_, err := wtSvc.Add(ctx, model.WorktreeAddOptions{
		Path: "/tmp/wt-res", TaskID: "MGIT-8.4", AgentID: "a1",
	})
	require.NoError(t, err)

	wt, err := wtSvc.Resolve(ctx, "/tmp/wt-res")
	require.NoError(t, err)
	assert.Equal(t, "MGIT-8.4", wt.TaskID)
}

func TestWorktreeService_Add_InvalidTaskID(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()
	clock := fixedClock()

	wtSvc := NewWorktreeService(env.idx, env.branch, clock)

	_, err := wtSvc.Add(ctx, model.WorktreeAddOptions{
		Path: "/tmp/wt-bad", TaskID: "invalid",
	})
	assert.Error(t, err)
}

// --- ConfigService gaps ---

func TestConfigService_GetAll(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	svc, err := NewConfigService(configPath)
	require.NoError(t, err)

	cfg := svc.GetAll()
	assert.Equal(t, "MGIT", cfg.Project.Prefix)
	assert.Equal(t, 6860, cfg.API.HTTPPort)
}

// --- VerifyService gaps ---

func TestVerifyService_VerifyTaskCommits_Empty(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	verifySvc := NewVerifyService(env.cs, env.idx)
	err := verifySvc.VerifyTaskCommits(ctx, "MGIT-99.9")
	assert.NoError(t, err, "empty task should pass verification")
}

// --- RollbackService gaps: invertDiffs coverage ---

func TestRollbackService_InvertDiffs_Deleted(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	_, err := env.commit.CreateCommit(ctx, CreateCommitRequest{
		TaskID: "MGIT-6.5", AgentID: "a",
		FileDiffs: []model.FileDiff{
			{Path: "del.go", Operation: model.DiffDeleted, OldHash: "old"},
		},
	})
	require.NoError(t, err)

	revert, err := env.rollbk.RollbackTask(ctx, RollbackRequest{
		TaskID: "MGIT-6.5", DryRun: true,
	})
	require.NoError(t, err)
	assert.Equal(t, model.CommitTypeRollback, revert.CommitType)
}

func TestRollbackService_InvertDiffs_Modified(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	_, err := env.commit.CreateCommit(ctx, CreateCommitRequest{
		TaskID: "MGIT-6.6", AgentID: "a",
		FileDiffs: []model.FileDiff{
			{Path: "mod.go", Operation: model.DiffModified, OldHash: "o", NewHash: "n"},
		},
	})
	require.NoError(t, err)

	revert, err := env.rollbk.RollbackTask(ctx, RollbackRequest{
		TaskID: "MGIT-6.6", DryRun: true,
	})
	require.NoError(t, err)
	assert.Equal(t, model.CommitTypeRollback, revert.CommitType)
}

// --- SquashService gaps ---

func TestSquashService_CustomMessage(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	_, err := env.commit.CreateCommit(ctx, CreateCommitRequest{
		TaskID: "MGIT-5.4", AgentID: "a", Message: "c1",
	})
	require.NoError(t, err)

	squashed, err := env.squash.SquashTask(ctx, SquashRequest{
		TaskID: "MGIT-5.4", Message: "custom squash message",
	})
	require.NoError(t, err)
	assert.Contains(t, squashed.Message, "custom squash message")
}

// --- AuditService: ExportAuditLog error path ---

func TestAuditService_ExportAuditLog_Empty(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "audit.log")
	svc := NewAuditService(logPath, fixedClock())

	data, err := svc.ExportAuditLog(AuditFilters{})
	require.NoError(t, err)
	assert.Contains(t, string(data), "null")
}
