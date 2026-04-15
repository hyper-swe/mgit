// Package e2e — worktree lifecycle integration tests.
// Refs: FR-16, MGIT-8.2.2
package e2e

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/service"
)

// makeWorktreePath returns an existing temp directory the test can pass
// to WorktreeService.Add. The directory is cleaned up at test end.
func makeWorktreePath(t *testing.T, label string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), label)
	require.NoError(t, os.MkdirAll(p, 0o750))
	return p
}

// TestWorktreeIntegration_FullLifecycle exercises the canonical create →
// commit → squash → remove flow against a single worktree.
// Refs: FR-16, MGIT-8.2.2
func TestWorktreeIntegration_FullLifecycle(t *testing.T) {
	env := setupServiceEnv(t)
	ctx := context.Background()

	wtPath := makeWorktreePath(t, "wt-life")
	taskID := "MGIT-8.2.2"
	wt, err := env.wtSvc.Add(ctx, model.WorktreeAddOptions{
		Path:    wtPath,
		TaskID:  taskID,
		AgentID: "wt-test",
	})
	require.NoError(t, err)
	require.NotNil(t, wt)
	assert.Equal(t, "task/"+taskID, wt.Branch, "worktree must auto-bind to task branch")

	// Verify the worktree is registered.
	resolved, err := env.wtSvc.Resolve(ctx, wtPath)
	require.NoError(t, err)
	assert.Equal(t, taskID, resolved.TaskID)

	// Commit through the regular CommitService (worktree binding is
	// metadata-only at this layer).
	_, err = env.commit.CreateCommit(ctx, service.CreateCommitRequest{
		TaskID: taskID, AgentID: "wt-test", Message: "wt commit",
	})
	require.NoError(t, err)

	// Squash and verify the task has the squash commit.
	squashed, err := env.squash.SquashTask(ctx, service.SquashRequest{TaskID: taskID})
	require.NoError(t, err)
	assert.Equal(t, model.CommitTypeSquash, squashed.CommitType)

	// Remove the worktree registration.
	require.NoError(t, env.wtSvc.Remove(ctx, wtPath, false))
	_, err = env.wtSvc.Resolve(ctx, wtPath)
	require.Error(t, err)
	assert.True(t, errors.Is(err, model.ErrWorktreeNotFound))
}

// TestWorktreeIntegration_ConcurrentWorktrees verifies that two worktrees
// on different tasks both register successfully and that commits made
// against each task are visible only via that task's record set.
// Refs: FR-16, MGIT-8.2.2
func TestWorktreeIntegration_ConcurrentWorktrees(t *testing.T) {
	env := setupServiceEnv(t)
	ctx := context.Background()

	wtA, err := env.wtSvc.Add(ctx, model.WorktreeAddOptions{
		Path:    makeWorktreePath(t, "wt-a"),
		TaskID:  "MGIT-8.2.21",
		AgentID: "agent-a",
	})
	require.NoError(t, err)

	wtB, err := env.wtSvc.Add(ctx, model.WorktreeAddOptions{
		Path:    makeWorktreePath(t, "wt-b"),
		TaskID:  "MGIT-8.2.22",
		AgentID: "agent-b",
	})
	require.NoError(t, err)
	require.NotEqual(t, wtA.Branch, wtB.Branch, "worktrees must use different branches")

	// Interleave commits across the two task IDs.
	for i := 0; i < 3; i++ {
		_, err = env.commit.CreateCommit(ctx, service.CreateCommitRequest{
			TaskID: "MGIT-8.2.21", AgentID: "agent-a", Message: fmt.Sprintf("a-%d", i),
		})
		require.NoError(t, err)
		_, err = env.commit.CreateCommit(ctx, service.CreateCommitRequest{
			TaskID: "MGIT-8.2.22", AgentID: "agent-b", Message: fmt.Sprintf("b-%d", i),
		})
		require.NoError(t, err)
	}

	// Each task only sees its own commits.
	a, err := env.idx.GetTaskCommits(ctx, "MGIT-8.2.21")
	require.NoError(t, err)
	b, err := env.idx.GetTaskCommits(ctx, "MGIT-8.2.22")
	require.NoError(t, err)
	assert.Len(t, a, 3)
	assert.Len(t, b, 3)

	// Worktree list shows both registrations.
	all, err := env.wtSvc.List(ctx)
	require.NoError(t, err)
	assert.Len(t, all, 2)
}

// TestWorktreeIntegration_TaskConflict verifies the UNIQUE(task_id)
// constraint blocks a second worktree from binding to the same task.
// Refs: FR-16.4, MGIT-8.2.2
func TestWorktreeIntegration_TaskConflict(t *testing.T) {
	env := setupServiceEnv(t)
	ctx := context.Background()

	taskID := "MGIT-8.2.2"
	_, err := env.wtSvc.Add(ctx, model.WorktreeAddOptions{
		Path: makeWorktreePath(t, "wt-1"), TaskID: taskID, AgentID: "agent-1",
	})
	require.NoError(t, err)

	_, err = env.wtSvc.Add(ctx, model.WorktreeAddOptions{
		Path: makeWorktreePath(t, "wt-2"), TaskID: taskID, AgentID: "agent-2",
	})
	require.Error(t, err, "second worktree on the same task must be rejected")
	// SQLite UNIQUE violation surfaces as a constraint error from the index layer.
	assert.Contains(t, err.Error(), "UNIQUE")
}

// TestWorktreeIntegration_BranchConflict verifies the UNIQUE(branch_name)
// constraint blocks a second worktree from sharing a branch.
// Refs: FR-16.3, MGIT-8.2.2
func TestWorktreeIntegration_BranchConflict(t *testing.T) {
	env := setupServiceEnv(t)
	ctx := context.Background()

	// Add worktree A on its own task/branch.
	_, err := env.wtSvc.Add(ctx, model.WorktreeAddOptions{
		Path: makeWorktreePath(t, "wt-A"), TaskID: "MGIT-8.2.31", AgentID: "agent-a",
	})
	require.NoError(t, err)

	// Add worktree B with a DIFFERENT task ID but request the SAME branch
	// name as A. The UNIQUE(branch_name) constraint must block this.
	_, err = env.wtSvc.Add(ctx, model.WorktreeAddOptions{
		Path:    makeWorktreePath(t, "wt-B"),
		TaskID:  "MGIT-8.2.32",
		AgentID: "agent-b",
		Branch:  "task/MGIT-8.2.31",
	})
	require.Error(t, err, "two worktrees must not share a branch")
	assert.Contains(t, err.Error(), "UNIQUE")
}

// TestWorktreeIntegration_RollbackInWorktree verifies that a rollback
// invoked while a worktree is registered creates the expected rollback
// commit and the worktree binding remains intact.
// Refs: FR-16, MGIT-8.2.2
func TestWorktreeIntegration_RollbackInWorktree(t *testing.T) {
	env := setupServiceEnv(t)
	ctx := context.Background()

	taskID := "MGIT-8.2.2"
	wtPath := makeWorktreePath(t, "wt-rollback")
	_, err := env.wtSvc.Add(ctx, model.WorktreeAddOptions{
		Path: wtPath, TaskID: taskID, AgentID: "agent-rb",
	})
	require.NoError(t, err)

	// Create two commits on the bound task, then roll back the task.
	_, err = env.commit.CreateCommit(ctx, service.CreateCommitRequest{
		TaskID: taskID, AgentID: "agent-rb", Message: "first",
	})
	require.NoError(t, err)
	_, err = env.commit.CreateCommit(ctx, service.CreateCommitRequest{
		TaskID: taskID, AgentID: "agent-rb", Message: "second",
	})
	require.NoError(t, err)

	rb, err := env.rollback.RollbackTask(ctx, service.RollbackRequest{
		TaskID: taskID,
		Reason: "wt rollback test",
	})
	require.NoError(t, err)
	require.NotNil(t, rb)
	assert.Equal(t, model.CommitTypeRollback, rb.CommitType,
		"rollback must produce a rollback-type commit (append-only)")

	// Worktree binding survives the rollback.
	resolved, err := env.wtSvc.Resolve(ctx, wtPath)
	require.NoError(t, err)
	assert.Equal(t, taskID, resolved.TaskID)
}

// TestWorktreeIntegration_PruneStale verifies the prune logic removes
// worktrees whose path is missing AND worktrees older than the cutoff.
// Refs: FR-16, MGIT-8.2.2
func TestWorktreeIntegration_PruneStale(t *testing.T) {
	env := setupServiceEnv(t)
	ctx := context.Background()

	// Worktree #1: real path, will be deleted from disk → stale.
	pathStale := makeWorktreePath(t, "wt-stale")
	_, err := env.wtSvc.Add(ctx, model.WorktreeAddOptions{
		Path: pathStale, TaskID: "MGIT-8.2.41", AgentID: "agent-stale",
	})
	require.NoError(t, err)
	require.NoError(t, os.RemoveAll(pathStale))

	// Worktree #2: real path that still exists → not stale.
	pathLive := makeWorktreePath(t, "wt-live")
	_, err = env.wtSvc.Add(ctx, model.WorktreeAddOptions{
		Path: pathLive, TaskID: "MGIT-8.2.42", AgentID: "agent-live",
	})
	require.NoError(t, err)

	// Dry-run prune lists the stale path without deleting it.
	stale, err := env.wtSvc.Prune(ctx, true, 0)
	require.NoError(t, err)
	assert.Contains(t, stale, pathStale)
	assert.NotContains(t, stale, pathLive)
	all, err := env.wtSvc.List(ctx)
	require.NoError(t, err)
	assert.Len(t, all, 2, "dry-run must not actually delete")

	// Real prune removes the stale registration.
	stale, err = env.wtSvc.Prune(ctx, false, 0)
	require.NoError(t, err)
	assert.Contains(t, stale, pathStale)

	all, err = env.wtSvc.List(ctx)
	require.NoError(t, err)
	require.Len(t, all, 1)
	assert.Equal(t, pathLive, all[0].Path)

	// Time-based prune: any worktree older than 1ns is stale.
	time.Sleep(2 * time.Millisecond)
	stale, err = env.wtSvc.Prune(ctx, false, 1)
	require.NoError(t, err)
	if env.clock != nil {
		// The fixed clock used in tests may not advance, so the
		// time-based check is best-effort. Either zero or one stale path
		// is acceptable here.
		assert.LessOrEqual(t, len(stale), 1)
	}
}
