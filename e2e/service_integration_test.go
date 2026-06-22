// Package e2e contains end-to-end integration tests for mgit.
// These tests verify services work together in realistic workflows.
// Refs: MGIT-3.3.1, MGIT-3.3.2, MGIT-3.3.3, MGIT-3.3.4
package e2e

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/service"
	gitstore "github.com/hyper-swe/mgit/internal/store/git"
	"github.com/hyper-swe/mgit/internal/store/index"
)

type serviceEnv struct {
	repo     *gitstore.Repository
	idx      *index.Store
	commit   *service.CommitService
	squash   *service.SquashService
	rollback *service.RollbackService
	branch   *service.BranchService
	verify   *service.VerifyService
	audit    *service.AuditService
	diff     *service.DiffService
	checkout *service.CheckoutService
	worktree *gitstore.WorktreeStore
	merge    *service.MergeService
	gc       *service.GCService
	bundle   *service.BundleService
	wtSvc    *service.WorktreeService
	clock    func() time.Time
}

func setupServiceEnv(t *testing.T) *serviceEnv {
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

	cs := gitstore.NewCommitStore(repo)
	bs := gitstore.NewBranchStore(repo)
	ds := gitstore.NewDiffStore(repo)
	ws := gitstore.NewWorktreeStore(repo)
	ms := gitstore.NewMergeStore(repo)
	gcs := gitstore.NewGCStore(repo)
	auditPath := filepath.Join(tmpDir, ".mgit", "audit.log")

	return &serviceEnv{
		repo:     repo,
		idx:      idx,
		commit:   service.NewCommitService(repo, cs, idx),
		squash:   service.NewSquashService(repo, cs, idx),
		rollback: service.NewRollbackService(repo, cs, idx),
		branch:   service.NewBranchService(repo, bs, idx),
		verify:   service.NewVerifyService(cs, idx),
		audit:    service.NewAuditService(auditPath, clock),
		diff:     service.NewDiffService(ds, cs, idx),
		checkout: service.NewCheckoutService(bs, ws),
		worktree: ws,
		merge:    service.NewMergeService(repo, bs, ms, cs),
		gc:       service.NewGCService(gcs),
		bundle:   service.NewBundleService(idx, clock),
		wtSvc:    service.NewWorktreeService(idx, service.NewBranchService(repo, bs, idx), ws, clock),
		clock:    clock,
	}
}

// --- MGIT-3.3.1: Commit + Squash Integration ---

func TestIntegration_CommitAndSquash_Successful(t *testing.T) {
	env := setupServiceEnv(t)
	ctx := context.Background()

	// 1. Create 5 commits
	commitHashes := make([]string, 5)
	for i := range 5 {
		c, err := env.commit.CreateCommit(ctx, service.CreateCommitRequest{
			TaskID:  "MGIT-1.2.3",
			AgentID: "agent-01",
			Message: fmt.Sprintf("micro commit %d", i+1),
		})
		require.NoError(t, err)
		commitHashes[i] = c.CommitID
	}

	// 2. Squash
	squashed, err := env.squash.SquashTask(ctx, service.SquashRequest{
		TaskID: "MGIT-1.2.3",
	})
	require.NoError(t, err)
	assert.Equal(t, model.CommitTypeSquash, squashed.CommitType)
	assert.Contains(t, squashed.Message, "Squashed from 5 micro-commits")

	// 3. Verify squashed commit exists
	assert.NotEmpty(t, squashed.CommitID)

	// 4. Verify original 5 still in index (append-only)
	records, err := env.idx.GetTaskCommits(ctx, "MGIT-1.2.3")
	require.NoError(t, err)
	assert.Len(t, records, 6, "5 originals + 1 squash must all exist")

	// 5. Verify all original commits are retrievable
	for _, hash := range commitHashes {
		_, err := env.commit.GetCommit(ctx, hash)
		assert.NoError(t, err, "original commit %s must still exist", hash[:8])
	}
}

// --- MGIT-3.3.2: Rollback + Verify Integration ---

func TestIntegration_RollbackAndVerify_Successful(t *testing.T) {
	env := setupServiceEnv(t)
	ctx := context.Background()

	// 1. Create 3 commits
	for i := range 3 {
		_, err := env.commit.CreateCommit(ctx, service.CreateCommitRequest{
			TaskID:  "MGIT-2.1.1",
			AgentID: "agent-01",
			Message: fmt.Sprintf("commit %d", i+1),
		})
		require.NoError(t, err)
	}

	// 2. Dry-run rollback
	dryResult, err := env.rollback.RollbackTask(ctx, service.RollbackRequest{
		TaskID: "MGIT-2.1.1",
		DryRun: true,
	})
	require.NoError(t, err)
	assert.Equal(t, model.CommitTypeRollback, dryResult.CommitType)

	// 3. Verify dry-run didn't change anything
	recordsBefore, err := env.idx.GetTaskCommits(ctx, "MGIT-2.1.1")
	require.NoError(t, err)
	assert.Len(t, recordsBefore, 3, "dry-run must not add commits")

	// 4. Actual rollback
	revert, err := env.rollback.RollbackTask(ctx, service.RollbackRequest{
		TaskID: "MGIT-2.1.1",
		Reason: "test rollback",
	})
	require.NoError(t, err)
	assert.Equal(t, model.CommitTypeRollback, revert.CommitType)
	assert.Contains(t, revert.Message, "Revert")

	// 5. Verify index has 4 entries (3 + revert)
	recordsAfter, err := env.idx.GetTaskCommits(ctx, "MGIT-2.1.1")
	require.NoError(t, err)
	assert.Len(t, recordsAfter, 4, "3 originals + 1 revert")

	// 6. Verify task commits via VerifyService
	err = env.verify.VerifyTaskCommits(ctx, "MGIT-2.1.1")
	assert.NoError(t, err, "task commits must all be valid after rollback")
}

// --- MGIT-3.3.3: Branch + Commit Integration ---

func TestIntegration_BranchAndCommit_Successful(t *testing.T) {
	env := setupServiceEnv(t)
	ctx := context.Background()

	// 1. Create branch
	branch, err := env.branch.CreateBranch(ctx, "MGIT-3.1")
	require.NoError(t, err)
	assert.Equal(t, "task/MGIT-3.1", branch.Name)

	// 2. Switch to branch
	err = env.branch.SwitchBranch(ctx, "task/MGIT-3.1")
	require.NoError(t, err)

	// 3. Lock branch
	err = env.branch.LockBranch(ctx, "task/MGIT-3.1", "agent-01", 30*time.Second)
	require.NoError(t, err)

	// 4. Create commits on branch
	for i := range 2 {
		_, err := env.commit.CreateCommit(ctx, service.CreateCommitRequest{
			TaskID:  "MGIT-3.1",
			AgentID: "agent-01",
			Message: fmt.Sprintf("branch commit %d", i+1),
			Branch:  "task/MGIT-3.1",
		})
		require.NoError(t, err)
	}

	// 5. Verify commits are indexed
	records, err := env.idx.GetTaskCommits(ctx, "MGIT-3.1")
	require.NoError(t, err)
	assert.Len(t, records, 2)

	// 6. Unlock
	err = env.branch.UnlockBranch(ctx, "task/MGIT-3.1")
	require.NoError(t, err)

	// 7. Switch back to main
	err = env.branch.SwitchBranch(ctx, "main")
	require.NoError(t, err)

	// 8. List branches
	branches, err := env.branch.ListBranches(ctx)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(branches), 2, "must have main + task branch")
}

// --- MGIT-3.3.4: Full Workflow ---

func TestIntegration_FullWorkflow_EndToEnd(t *testing.T) {
	env := setupServiceEnv(t)
	ctx := context.Background()

	// 1. Create branch for task
	_, err := env.branch.CreateBranch(ctx, "MGIT-4.1")
	require.NoError(t, err)

	// 2. Log branch creation in audit
	require.NoError(t, env.audit.LogOperation(service.AuditEntry{
		Operation: service.AuditBranchCreate,
		AgentID:   "agent-01",
		TaskID:    "MGIT-4.1",
		Details:   "created task branch",
	}))

	// 3. Switch to branch
	require.NoError(t, env.branch.SwitchBranch(ctx, "task/MGIT-4.1"))

	// 4. Make 5 commits
	for i := range 5 {
		c, err := env.commit.CreateCommit(ctx, service.CreateCommitRequest{
			TaskID:  "MGIT-4.1",
			AgentID: "agent-01",
			Message: fmt.Sprintf("implement step %d", i+1),
			Branch:  "task/MGIT-4.1",
		})
		require.NoError(t, err)

		require.NoError(t, env.audit.LogOperation(service.AuditEntry{
			Operation: service.AuditCreateCommit,
			AgentID:   "agent-01",
			TaskID:    "MGIT-4.1",
			CommitID:  c.CommitID,
		}))
	}

	// 5. Verify task commits
	err = env.verify.VerifyTaskCommits(ctx, "MGIT-4.1")
	require.NoError(t, err)

	// 6. Squash
	squashed, err := env.squash.SquashTask(ctx, service.SquashRequest{
		TaskID: "MGIT-4.1",
	})
	require.NoError(t, err)
	assert.Equal(t, model.CommitTypeSquash, squashed.CommitType)

	require.NoError(t, env.audit.LogOperation(service.AuditEntry{
		Operation: service.AuditSquash,
		AgentID:   "agent-01",
		TaskID:    "MGIT-4.1",
		CommitID:  squashed.CommitID,
	}))

	// 7. Verify complete audit trail
	entries, err := env.audit.GetAuditLog(service.AuditFilters{TaskID: "MGIT-4.1"})
	require.NoError(t, err)
	assert.Len(t, entries, 7, "1 branch_create + 5 commits + 1 squash = 7 audit entries")

	// 8. Verify index integrity
	records, err := env.idx.GetTaskCommits(ctx, "MGIT-4.1")
	require.NoError(t, err)
	assert.Len(t, records, 6, "5 originals + 1 squash")

	// 9. Verify all operations were by agent-01
	for _, entry := range entries {
		assert.Equal(t, "agent-01", entry.AgentID)
		assert.Equal(t, "MGIT-4.1", entry.TaskID)
	}
}
