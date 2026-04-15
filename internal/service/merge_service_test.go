package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gitstore "github.com/hyper-swe/mgit/internal/store/git"
)

// --- MergeService Tests ---
// Refs: FR-8.4, MGIT-4.2.10

func setupMergeEnv(t *testing.T) (*testEnv, *MergeService) {
	t.Helper()
	env := setupTestEnv(t)
	ms := gitstore.NewMergeStore(env.repo)
	mergeSvc := NewMergeService(env.repo, env.bs, ms, env.cs)
	return env, mergeSvc
}

func TestMergeService_Merge_EmptySourceBranch(t *testing.T) {
	_, mergeSvc := setupMergeEnv(t)
	ctx := context.Background()

	_, err := mergeSvc.Merge(ctx, MergeRequest{SourceBranch: ""})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "source branch must not be empty")
}

func TestMergeService_Merge_NonexistentBranch(t *testing.T) {
	_, mergeSvc := setupMergeEnv(t)
	ctx := context.Background()

	_, err := mergeSvc.Merge(ctx, MergeRequest{SourceBranch: "nonexistent"})
	assert.Error(t, err)
}

func TestMergeService_Merge_FastForward(t *testing.T) {
	env, mergeSvc := setupMergeEnv(t)
	ctx := context.Background()

	// Create a task branch with a commit, then switch back to main.
	_, err := env.branch.CreateBranch(ctx, "MGIT-10.1")
	require.NoError(t, err)

	err = env.branch.SwitchBranch(ctx, "task/MGIT-10.1")
	require.NoError(t, err)

	_, err = env.commit.CreateCommit(ctx, CreateCommitRequest{
		TaskID: "MGIT-10.1", AgentID: "a", Message: "feature commit",
	})
	require.NoError(t, err)

	err = env.branch.SwitchBranch(ctx, "main")
	require.NoError(t, err)

	// Merge with auto strategy: should fast-forward because main hasn't diverged.
	result, err := mergeSvc.Merge(ctx, MergeRequest{
		SourceBranch: "task/MGIT-10.1",
		Strategy:     MergeAuto,
	})
	require.NoError(t, err)
	assert.True(t, result.FastFwd)
	assert.Equal(t, "fast-forward", result.Status)
	assert.Equal(t, MergeAuto, result.Strategy)
	assert.Equal(t, "task/MGIT-10.1", result.Source)
}

func TestMergeService_Merge_NoFF(t *testing.T) {
	env, mergeSvc := setupMergeEnv(t)
	ctx := context.Background()

	// Create a task branch with a commit.
	_, err := env.branch.CreateBranch(ctx, "MGIT-10.2")
	require.NoError(t, err)
	err = env.branch.SwitchBranch(ctx, "task/MGIT-10.2")
	require.NoError(t, err)

	_, err = env.commit.CreateCommit(ctx, CreateCommitRequest{
		TaskID: "MGIT-10.2", AgentID: "a", Message: "branch commit",
	})
	require.NoError(t, err)

	err = env.branch.SwitchBranch(ctx, "main")
	require.NoError(t, err)

	// Merge with no-ff: should create a merge commit even when ff is possible.
	result, err := mergeSvc.Merge(ctx, MergeRequest{
		SourceBranch: "task/MGIT-10.2",
		Strategy:     MergeNoFF,
	})
	require.NoError(t, err)
	assert.False(t, result.FastFwd)
	assert.Equal(t, "merged", result.Status)
	assert.Equal(t, MergeNoFF, result.Strategy)
	assert.NotEmpty(t, result.MergedHash)
}

func TestMergeService_Merge_Squash(t *testing.T) {
	env, mergeSvc := setupMergeEnv(t)
	ctx := context.Background()

	// Create a task branch with commits.
	_, err := env.branch.CreateBranch(ctx, "MGIT-10.3")
	require.NoError(t, err)
	err = env.branch.SwitchBranch(ctx, "task/MGIT-10.3")
	require.NoError(t, err)

	_, err = env.commit.CreateCommit(ctx, CreateCommitRequest{
		TaskID: "MGIT-10.3", AgentID: "a", Message: "squash me",
	})
	require.NoError(t, err)

	err = env.branch.SwitchBranch(ctx, "main")
	require.NoError(t, err)

	result, err := mergeSvc.Merge(ctx, MergeRequest{
		SourceBranch: "task/MGIT-10.3",
		Strategy:     MergeSquash,
	})
	require.NoError(t, err)
	assert.Equal(t, "squashed", result.Status)
	assert.Equal(t, MergeSquash, result.Strategy)
	assert.NotEmpty(t, result.MergedHash)
}

func TestMergeService_Merge_UnknownStrategy(t *testing.T) {
	env, mergeSvc := setupMergeEnv(t)
	ctx := context.Background()

	_, err := env.branch.CreateBranch(ctx, "MGIT-10.4")
	require.NoError(t, err)

	_, err = mergeSvc.Merge(ctx, MergeRequest{
		SourceBranch: "task/MGIT-10.4",
		Strategy:     MergeStrategy("invalid-strategy"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unknown strategy")
}

func TestMergeService_Merge_DefaultStrategy(t *testing.T) {
	env, mergeSvc := setupMergeEnv(t)
	ctx := context.Background()

	// When strategy is empty, it defaults to MergeAuto.
	_, err := env.branch.CreateBranch(ctx, "MGIT-10.5")
	require.NoError(t, err)
	err = env.branch.SwitchBranch(ctx, "task/MGIT-10.5")
	require.NoError(t, err)

	_, err = env.commit.CreateCommit(ctx, CreateCommitRequest{
		TaskID: "MGIT-10.5", AgentID: "a", Message: "auto",
	})
	require.NoError(t, err)
	err = env.branch.SwitchBranch(ctx, "main")
	require.NoError(t, err)

	result, err := mergeSvc.Merge(ctx, MergeRequest{
		SourceBranch: "task/MGIT-10.5",
		// Strategy left empty => defaults to auto
	})
	require.NoError(t, err)
	assert.Equal(t, MergeAuto, result.Strategy)
}

func TestMergeService_Merge_CustomMessage(t *testing.T) {
	env, mergeSvc := setupMergeEnv(t)
	ctx := context.Background()

	_, err := env.branch.CreateBranch(ctx, "MGIT-10.6")
	require.NoError(t, err)
	err = env.branch.SwitchBranch(ctx, "task/MGIT-10.6")
	require.NoError(t, err)
	_, err = env.commit.CreateCommit(ctx, CreateCommitRequest{
		TaskID: "MGIT-10.6", AgentID: "a", Message: "work",
	})
	require.NoError(t, err)
	err = env.branch.SwitchBranch(ctx, "main")
	require.NoError(t, err)

	result, err := mergeSvc.Merge(ctx, MergeRequest{
		SourceBranch: "task/MGIT-10.6",
		Strategy:     MergeNoFF,
		Message:      "custom merge message",
	})
	require.NoError(t, err)
	assert.Equal(t, "merged", result.Status)
	assert.NotEmpty(t, result.MergedHash)
}

func TestMergeService_Merge_SquashCustomMessage(t *testing.T) {
	env, mergeSvc := setupMergeEnv(t)
	ctx := context.Background()

	_, err := env.branch.CreateBranch(ctx, "MGIT-10.7")
	require.NoError(t, err)
	err = env.branch.SwitchBranch(ctx, "task/MGIT-10.7")
	require.NoError(t, err)
	_, err = env.commit.CreateCommit(ctx, CreateCommitRequest{
		TaskID: "MGIT-10.7", AgentID: "a", Message: "squash work",
	})
	require.NoError(t, err)
	err = env.branch.SwitchBranch(ctx, "main")
	require.NoError(t, err)

	result, err := mergeSvc.Merge(ctx, MergeRequest{
		SourceBranch: "task/MGIT-10.7",
		Strategy:     MergeSquash,
		Message:      "custom squash message",
	})
	require.NoError(t, err)
	assert.Equal(t, "squashed", result.Status)
}
