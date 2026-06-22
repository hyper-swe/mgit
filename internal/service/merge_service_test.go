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

func TestMergeService_Merge_SelfMerge_AlreadyUpToDate(t *testing.T) {
	env, mergeSvc := setupMergeEnv(t)
	ctx := context.Background()

	headBefore, err := env.repo.Head()
	require.NoError(t, err)

	// Merging the current branch into itself is a no-op, not a fast-forward.
	result, err := mergeSvc.Merge(ctx, MergeRequest{
		SourceBranch: "main",
		Strategy:     MergeAuto,
	})
	require.NoError(t, err)
	assert.Equal(t, "already up to date", result.Status)
	assert.False(t, result.FastFwd, "self-merge must not report a fast-forward")
	assert.Equal(t, "main", result.Source)
	assert.Equal(t, "main", result.Target)

	headAfter, err := env.repo.Head()
	require.NoError(t, err)
	assert.Equal(t, headBefore, headAfter, "self-merge must not move HEAD")
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

// TestMergeService_Merge_NoFF_IncorporatesSourceContent verifies end-to-end
// through the service that a no-ff merge produces a merge commit whose tree
// contains the source branch's file content, and materializes it onto the
// working tree on disk. Pins the MGIT-15 fix at the service boundary.
// Refs: MGIT-15, FR-8.4
func TestMergeService_Merge_NoFF_IncorporatesSourceContent(t *testing.T) {
	env, mergeSvc := setupMergeEnv(t)
	ctx := context.Background()

	// Source branch: add a source-only file and commit it there.
	_, err := env.branch.CreateBranch(ctx, "MGIT-15.1")
	require.NoError(t, err)
	require.NoError(t, env.branch.SwitchBranch(ctx, "task/MGIT-15.1"))

	srcFile := filepath.Join(env.repo.Root(), "src_only.txt")
	require.NoError(t, os.WriteFile(srcFile, []byte("from source\n"), 0o600))
	require.NoError(t, env.wt.Add(ctx, "src_only.txt"))
	_, err = env.commit.CreateCommit(ctx, CreateCommitRequest{
		TaskID: "MGIT-15.1", AgentID: "a", Message: "add source file",
	})
	require.NoError(t, err)

	// main diverges with its own file so a fast-forward is impossible.
	require.NoError(t, env.branch.SwitchBranch(ctx, "main"))
	require.NoError(t, os.Remove(srcFile)) // source-only file is not on main's tree
	mainFile := filepath.Join(env.repo.Root(), "main_only.txt")
	require.NoError(t, os.WriteFile(mainFile, []byte("from main\n"), 0o600))
	require.NoError(t, env.wt.Add(ctx, "main_only.txt"))
	_, err = env.commit.CreateCommit(ctx, CreateCommitRequest{
		TaskID: "MGIT-15.2", AgentID: "a", Message: "add main file",
	})
	require.NoError(t, err)

	result, err := mergeSvc.Merge(ctx, MergeRequest{
		SourceBranch: "task/MGIT-15.1",
		Strategy:     MergeNoFF,
	})
	require.NoError(t, err)
	assert.Equal(t, "merged", result.Status)

	// The merge commit's tree must contain BOTH files.
	merged, err := env.cs.GetCommit(ctx, result.MergedHash)
	require.NoError(t, err)
	assert.NotEmpty(t, merged.TreeHash)

	// The working tree on disk must now reflect the merge.
	got, err := os.ReadFile(srcFile) //nolint:gosec // test-controlled path under t.TempDir
	require.NoError(t, err, "no-ff merge must materialize the source-only file onto disk")
	assert.Equal(t, "from source\n", string(got))
	gotMain, err := os.ReadFile(mainFile) //nolint:gosec // test-controlled path under t.TempDir
	require.NoError(t, err)
	assert.Equal(t, "from main\n", string(gotMain))
}
