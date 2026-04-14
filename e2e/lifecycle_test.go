// Package e2e contains end-to-end tests for mgit.
// These tests exercise the full stack: service → store → go-git + SQLite.
// Refs: MGIT-6.1.1, MGIT-6.1.2, MGIT-6.1.3, MGIT-6.1.4
package e2e

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit-dev/internal/model"
	"github.com/hyper-swe/mgit-dev/internal/service"
)

// --- MGIT-6.1.1: Commit Lifecycle ---

func TestE2E_CommitLifecycle(t *testing.T) {
	env := setupServiceEnv(t)
	ctx := context.Background()

	// 1. Create commit
	c, err := env.commit.CreateCommit(ctx, service.CreateCommitRequest{
		TaskID:  "MGIT-1.2.3",
		AgentID: "e2e-agent",
		Message: "lifecycle test commit",
	})
	require.NoError(t, err)
	assert.NotEmpty(t, c.CommitID)
	assert.Contains(t, c.Message, "MGIT-1.2.3")

	// 2. Retrieve via log
	records, err := env.commit.GetTaskCommits(ctx, "MGIT-1.2.3")
	require.NoError(t, err)
	require.Len(t, records, 1)
	assert.Equal(t, c.CommitID, records[0].CommitHash)

	// 3. Show commit details
	retrieved, err := env.commit.GetCommit(ctx, c.CommitID)
	require.NoError(t, err)
	assert.Equal(t, c.CommitID, retrieved.CommitID)
	assert.Contains(t, retrieved.Message, "lifecycle test commit")

	// 4. Verify data persists (all fields populated)
	assert.NotEmpty(t, c.ContentHash)
	assert.NotEmpty(t, c.ParentID)
	assert.False(t, c.CreatedAt.IsZero())
}

// --- MGIT-6.1.2: Squash Workflow ---

func TestE2E_SquashWorkflow(t *testing.T) {
	env := setupServiceEnv(t)
	ctx := context.Background()

	// 1. Create 5 commits
	for i := range 5 {
		_, err := env.commit.CreateCommit(ctx, service.CreateCommitRequest{
			TaskID:  "MGIT-2.1",
			AgentID: "e2e-agent",
			Message: fmt.Sprintf("step %d", i+1),
		})
		require.NoError(t, err)
	}

	// 2. Verify 5 commits exist
	records, err := env.idx.GetTaskCommits(ctx, "MGIT-2.1")
	require.NoError(t, err)
	assert.Len(t, records, 5)

	// 3. Squash
	squashed, err := env.squash.SquashTask(ctx, service.SquashRequest{
		TaskID: "MGIT-2.1",
	})
	require.NoError(t, err)
	assert.Equal(t, model.CommitTypeSquash, squashed.CommitType)

	// 4. Verify 6 entries (5 original + 1 squash)
	records, err = env.idx.GetTaskCommits(ctx, "MGIT-2.1")
	require.NoError(t, err)
	assert.Len(t, records, 6)

	// 5. Verify chain integrity
	err = env.verify.VerifyTaskCommits(ctx, "MGIT-2.1")
	assert.NoError(t, err)
}

// --- MGIT-6.1.3: Rollback Workflow ---

func TestE2E_RollbackWorkflow(t *testing.T) {
	env := setupServiceEnv(t)
	ctx := context.Background()

	// 1. Create 3 commits
	for i := range 3 {
		_, err := env.commit.CreateCommit(ctx, service.CreateCommitRequest{
			TaskID:  "MGIT-3.1",
			AgentID: "e2e-agent",
			Message: fmt.Sprintf("commit %d", i+1),
		})
		require.NoError(t, err)
	}

	// 2. Rollback
	revert, err := env.rollback.RollbackTask(ctx, service.RollbackRequest{
		TaskID: "MGIT-3.1",
		Reason: "e2e test rollback",
	})
	require.NoError(t, err)
	assert.Equal(t, model.CommitTypeRollback, revert.CommitType)
	assert.Contains(t, revert.Message, "Revert")

	// 3. Verify 4 entries (3 original + 1 revert)
	records, err := env.idx.GetTaskCommits(ctx, "MGIT-3.1")
	require.NoError(t, err)
	assert.Len(t, records, 4)

	// 4. Verify all commits still exist (append-only)
	for _, rec := range records {
		_, err := env.commit.GetCommit(ctx, rec.CommitHash)
		assert.NoError(t, err)
	}
}

// --- MGIT-6.1.4: Branch Lifecycle ---

func TestE2E_BranchLifecycle(t *testing.T) {
	env := setupServiceEnv(t)
	ctx := context.Background()

	// 1. Create branch
	branch, err := env.branch.CreateBranch(ctx, "MGIT-4.1")
	require.NoError(t, err)
	assert.Equal(t, "task/MGIT-4.1", branch.Name)

	// 2. Switch to branch
	err = env.branch.SwitchBranch(ctx, "task/MGIT-4.1")
	require.NoError(t, err)

	// 3. Create commits on branch
	for i := range 3 {
		_, err := env.commit.CreateCommit(ctx, service.CreateCommitRequest{
			TaskID:  "MGIT-4.1",
			AgentID: "e2e-agent",
			Message: fmt.Sprintf("branch commit %d", i+1),
			Branch:  "task/MGIT-4.1",
		})
		require.NoError(t, err)
	}

	// 4. Squash
	squashed, err := env.squash.SquashTask(ctx, service.SquashRequest{
		TaskID: "MGIT-4.1",
	})
	require.NoError(t, err)
	assert.Equal(t, model.CommitTypeSquash, squashed.CommitType)

	// 5. Switch back to main
	err = env.branch.SwitchBranch(ctx, "main")
	require.NoError(t, err)

	// 6. List branches
	branches, err := env.branch.ListBranches(ctx)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(branches), 2)

	// 7. Lock and unlock
	err = env.branch.LockBranch(ctx, "task/MGIT-4.1", "e2e-agent", 30*time.Second)
	require.NoError(t, err)
	err = env.branch.UnlockBranch(ctx, "task/MGIT-4.1")
	require.NoError(t, err)
}
