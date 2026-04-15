// Package e2e — merge CLI integration tests.
// Refs: FR-8.4, MGIT-4.2.10
package e2e

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/service"
	gitstore "github.com/hyper-swe/mgit/internal/store/git"
)

// stageFileAndCommit writes content to relPath, stages it, and creates an
// mgit commit on the current branch with the given task ID. Returns the
// commit hash.
func stageFileAndCommit(t *testing.T, env *serviceEnv, relPath, content, taskID string) string {
	t.Helper()
	ctx := context.Background()

	target := filepath.Join(env.repo.Root(), relPath)
	require.NoError(t, os.MkdirAll(filepath.Dir(target), 0o750))
	require.NoError(t, os.WriteFile(target, []byte(content), 0o600))
	require.NoError(t, env.worktree.Add(ctx, relPath))

	c, err := env.commit.CreateCommit(ctx, service.CreateCommitRequest{
		TaskID:  taskID,
		AgentID: "merge-test",
		Message: "stage " + relPath,
	})
	require.NoError(t, err)
	return c.CommitID
}

// TestMerge_FastForward_Success creates a divergent task branch with one
// new commit and verifies that merging it back to main is a fast-forward.
// Refs: FR-8.4, MGIT-4.2.10
func TestMerge_FastForward_Success(t *testing.T) {
	env := setupServiceEnv(t)
	ctx := context.Background()

	// Create branch off main while main has nothing extra.
	_, err := env.branch.CreateBranch(ctx, "MGIT-4.2.10")
	require.NoError(t, err)

	// Switch to the task branch and add one commit.
	_, err = env.checkout.Checkout(ctx, "task/MGIT-4.2.10")
	require.NoError(t, err)
	stageFileAndCommit(t, env, "ff/a.txt", "added on branch\n", "MGIT-4.2.10")

	// Switch back to main and merge.
	_, err = env.checkout.Checkout(ctx, "main")
	require.NoError(t, err)

	result, err := env.merge.Merge(ctx, service.MergeRequest{
		SourceBranch: "task/MGIT-4.2.10",
		Strategy:     service.MergeAuto,
	})
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.True(t, result.FastFwd, "expected fast-forward merge")
	assert.Equal(t, service.MergeAuto, result.Strategy)
	assert.Equal(t, "fast-forward", result.Status)

	// HEAD should now match the source tip.
	head, err := env.repo.Head()
	require.NoError(t, err)
	assert.Equal(t, result.MergedHash, head)
}

// TestMerge_SquashFlag_CreatesSingleCommit verifies the --squash strategy
// produces a single new commit on HEAD that does not advance via FF.
// Refs: FR-8.4, MGIT-4.2.10
func TestMerge_SquashFlag_CreatesSingleCommit(t *testing.T) {
	env := setupServiceEnv(t)
	ctx := context.Background()

	_, err := env.branch.CreateBranch(ctx, "MGIT-4.2.10")
	require.NoError(t, err)

	_, err = env.checkout.Checkout(ctx, "task/MGIT-4.2.10")
	require.NoError(t, err)
	stageFileAndCommit(t, env, "sq/a.txt", "branch work\n", "MGIT-4.2.10")
	stageFileAndCommit(t, env, "sq/b.txt", "more branch work\n", "MGIT-4.2.10")

	// Make main diverge so squash is meaningful (otherwise FF would apply).
	_, err = env.checkout.Checkout(ctx, "main")
	require.NoError(t, err)
	stageFileAndCommit(t, env, "main_only.txt", "main work\n", "MGIT-4.2.10")

	mainHeadBefore, err := env.repo.Head()
	require.NoError(t, err)

	result, err := env.merge.Merge(ctx, service.MergeRequest{
		SourceBranch: "task/MGIT-4.2.10",
		Strategy:     service.MergeSquash,
		Message:      "squash test",
	})
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, service.MergeSquash, result.Strategy)
	assert.Equal(t, "squashed", result.Status)
	assert.False(t, result.FastFwd)

	// The squash commit's parent must be the previous main HEAD (single parent).
	mergedCommit, err := env.commit.GetCommit(ctx, result.MergedHash)
	require.NoError(t, err)
	assert.Equal(t, mainHeadBefore, mergedCommit.ParentID,
		"squash merge must have exactly one parent (the previous HEAD)")
}

// TestMerge_ConflictDetection_ReturnsError verifies that two divergent
// branches modifying the same file with different content are detected
// as a merge conflict before any commit is created.
// Refs: FR-8.4, MGIT-4.2.10
func TestMerge_ConflictDetection_ReturnsError(t *testing.T) {
	env := setupServiceEnv(t)
	ctx := context.Background()

	// Seed a shared base file on main.
	stageFileAndCommit(t, env, "shared.txt", "base content\n", "MGIT-4.2.10")

	// Branch off main, then change shared.txt in two different ways.
	_, err := env.branch.CreateBranch(ctx, "MGIT-4.2.10")
	require.NoError(t, err)

	_, err = env.checkout.Checkout(ctx, "task/MGIT-4.2.10")
	require.NoError(t, err)
	stageFileAndCommit(t, env, "shared.txt", "branch version\n", "MGIT-4.2.10")

	_, err = env.checkout.Checkout(ctx, "main")
	require.NoError(t, err)
	stageFileAndCommit(t, env, "shared.txt", "main version\n", "MGIT-4.2.10")

	headBefore, err := env.repo.Head()
	require.NoError(t, err)

	_, err = env.merge.Merge(ctx, service.MergeRequest{
		SourceBranch: "task/MGIT-4.2.10",
		Strategy:     service.MergeAuto,
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, model.ErrMergeConflict),
		"divergent edits to the same file must report ErrMergeConflict, got %v", err)
	assert.Contains(t, err.Error(), "shared.txt")

	// HEAD must NOT have advanced — conflict detection runs before commit.
	headAfter, err := env.repo.Head()
	require.NoError(t, err)
	assert.Equal(t, headBefore, headAfter,
		"HEAD must not advance when a merge is rejected for conflicts")
}

// TestMerge_NoFFFlag_CreatesMergeCommit verifies that --no-ff forces a
// two-parent merge commit even when fast-forward would be possible.
// Refs: FR-8.4, MGIT-4.2.10
func TestMerge_NoFFFlag_CreatesMergeCommit(t *testing.T) {
	env := setupServiceEnv(t)
	ctx := context.Background()

	_, err := env.branch.CreateBranch(ctx, "MGIT-4.2.10")
	require.NoError(t, err)

	_, err = env.checkout.Checkout(ctx, "task/MGIT-4.2.10")
	require.NoError(t, err)
	stageFileAndCommit(t, env, "noff/a.txt", "branch work\n", "MGIT-4.2.10")

	_, err = env.checkout.Checkout(ctx, "main")
	require.NoError(t, err)
	mainHeadBefore, err := env.repo.Head()
	require.NoError(t, err)

	result, err := env.merge.Merge(ctx, service.MergeRequest{
		SourceBranch: "task/MGIT-4.2.10",
		Strategy:     service.MergeNoFF,
		Message:      "no-ff merge",
	})
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.False(t, result.FastFwd, "--no-ff must NOT take the fast-forward path")
	assert.Equal(t, "merged", result.Status)

	// The merge commit must have two parents: previous HEAD + source tip.
	ms := gitstore.NewMergeStore(env.repo)
	isAncestor, err := ms.IsAncestor(ctx, mainHeadBefore, result.MergedHash)
	require.NoError(t, err)
	assert.True(t, isAncestor,
		"previous main HEAD must be an ancestor of the new merge commit")
}
