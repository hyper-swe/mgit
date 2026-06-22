package service

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
	gitstore "github.com/hyper-swe/mgit/internal/store/git"
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

// TestCommitService_GetCommit_PopulatesProvenance is the service-layer
// regression for MGIT-19: GetCommit must surface the task_id (from the message
// prefix) and the AUTHORITATIVE ADR-002 content_hash (joined from the index)
// that CreateCommit recorded — neither may read back blank. This is the
// provenance that show/log/cherry-pick depend on. Refs: MGIT-19, ADR-002
func TestCommitService_GetCommit_PopulatesProvenance(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	created, err := env.commit.CreateCommit(ctx, CreateCommitRequest{
		TaskID:  "MGIT-3.2",
		AgentID: "agent-01",
		Message: "implement provenance",
		FileDiffs: []model.FileDiff{
			{Path: "main.go", Operation: model.DiffAdded, NewHash: "abc"},
		},
	})
	require.NoError(t, err)
	require.NotEmpty(t, created.ContentHash)

	got, err := env.commit.GetCommit(ctx, created.CommitID)
	require.NoError(t, err)
	assert.Equal(t, "MGIT-3.2", got.TaskID.String(), "task_id must not read back blank")
	assert.Equal(t, created.ContentHash, got.ContentHash,
		"content_hash must equal the authoritative value recorded at create time")
}

// TestCommitService_GetCommit_AbbreviatedHash is the service-layer regression
// for MGIT-18: the abbreviated hash `mgit log` prints must resolve via the
// service read path. Refs: MGIT-18
func TestCommitService_GetCommit_AbbreviatedHash(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	created, err := env.commit.CreateCommit(ctx, CreateCommitRequest{
		TaskID: "MGIT-3.3", AgentID: "a", Message: "abbrev",
	})
	require.NoError(t, err)

	got, err := env.commit.GetCommit(ctx, created.CommitID[:8])
	require.NoError(t, err)
	assert.Equal(t, created.CommitID, got.CommitID)
}

// TestCommitService_ListCommits_PopulatesContentHash verifies the list read
// path binds the authoritative content_hash from the index for task commits
// (so `mgit log --json` no longer emits empty content_hash). Refs: MGIT-19
func TestCommitService_ListCommits_PopulatesContentHash(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	created, err := env.commit.CreateCommit(ctx, CreateCommitRequest{
		TaskID: "MGIT-3.4", AgentID: "a", Message: "listed",
	})
	require.NoError(t, err)

	commits, err := env.commit.ListCommits(ctx)
	require.NoError(t, err)
	var found *model.Commit
	for _, c := range commits {
		if c.CommitID == created.CommitID {
			found = c
			break
		}
	}
	require.NotNil(t, found, "created commit must appear in the log")
	assert.Equal(t, created.ContentHash, found.ContentHash)
	assert.Equal(t, "MGIT-3.4", found.TaskID.String())
}

// TestCommitService_GetCommit_NotFound verifies the store-error branch:
// an unknown hash surfaces ErrCommitNotFound from the read path. Refs: MGIT-18
func TestCommitService_GetCommit_NotFound(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	_, err := env.commit.GetCommit(ctx, "0000000000000000000000000000000000000000")
	assert.ErrorIs(t, err, model.ErrCommitNotFound)
}

// TestCommitService_GetCommit_IndexFailurePropagates verifies that a genuine
// index/DB failure during provenance enrichment is propagated (not silently
// swallowed as blank provenance) on the audit read path. A closed index turns
// the join into a real error rather than a not-found. Refs: MGIT-19
func TestCommitService_GetCommit_IndexFailurePropagates(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	created, err := env.commit.CreateCommit(ctx, CreateCommitRequest{
		TaskID: "MGIT-3.5", AgentID: "a", Message: "will fail enrich",
	})
	require.NoError(t, err)

	require.NoError(t, env.idx.Close()) // force a real DB error on the next query

	_, err = env.commit.GetCommit(ctx, created.CommitID)
	require.Error(t, err)
	assert.NotErrorIs(t, err, model.ErrTaskNotFound,
		"a real DB failure must not be reported as a missing index row")
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

// --- WorktreeService: Prune ---

func TestWorktreeService_Prune_NonexistentPath(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()
	clock := fixedClock()

	wtSvc := NewWorktreeService(env.idx, env.branch, clock)

	// Add a worktree with a path that does not exist on disk.
	_, err := wtSvc.Add(ctx, model.WorktreeAddOptions{
		Path: "/tmp/nonexistent-wt-prune-test", TaskID: "MGIT-15.1", AgentID: "a1",
	})
	require.NoError(t, err)

	// Prune should detect the stale worktree.
	stale, err := wtSvc.Prune(ctx, true, 0) // dryRun
	require.NoError(t, err)
	assert.Len(t, stale, 1)
	assert.Equal(t, "/tmp/nonexistent-wt-prune-test", stale[0])

	// Actual prune (not dry run).
	stale, err = wtSvc.Prune(ctx, false, 0)
	require.NoError(t, err)
	assert.Len(t, stale, 1)

	// After pruning, list should be empty.
	wts, err := wtSvc.List(ctx)
	require.NoError(t, err)
	assert.Empty(t, wts)
}

func TestWorktreeService_Prune_NoStale(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()
	clock := fixedClock()

	wtSvc := NewWorktreeService(env.idx, env.branch, clock)

	// No worktrees registered, so prune should return empty.
	stale, err := wtSvc.Prune(ctx, false, 0)
	require.NoError(t, err)
	assert.Empty(t, stale)
}

func TestWorktreeService_Prune_StaleAfterDuration(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	// Use a clock that returns a time far in the future so the worktree
	// will be older than any staleAfter duration.
	fixedTime := fixedClock()()
	futureClock := func() time.Time {
		return fixedTime.Add(48 * time.Hour)
	}

	wtSvc := NewWorktreeService(env.idx, env.branch, fixedClock())

	// Create a temp directory that actually exists.
	tmpPath := t.TempDir()
	_, err := wtSvc.Add(ctx, model.WorktreeAddOptions{
		Path: tmpPath, TaskID: "MGIT-15.2", AgentID: "a1",
	})
	require.NoError(t, err)

	// Switch to future clock for pruning.
	wtSvcFuture := NewWorktreeService(env.idx, env.branch, futureClock)

	// Prune with a staleAfter of 1 hour — worktree was created 48 hours ago.
	stale, err := wtSvcFuture.Prune(ctx, true, 1*time.Hour)
	require.NoError(t, err)
	assert.Len(t, stale, 1)
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
