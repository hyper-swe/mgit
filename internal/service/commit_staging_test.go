package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// These tests pin the contract MGIT-77 found missing: a commit must carry a
// change. Before the fix, CreateCommit happily wrote a commit whose tree was
// byte-identical to its parent's and returned a hash — the success signal —
// for work that was never recorded. Refs: FR-2, MGIT-77

// headAndParentTree returns the tree hash of the given commit, plus the
// tree hash of its parent — the two values whose equality means "this commit
// recorded nothing".
func headAndParentTree(t *testing.T, env *testEnv, commitID string) (string, string) {
	t.Helper()
	ctx := context.Background()
	c, err := env.cs.GetCommit(ctx, commitID)
	require.NoError(t, err)
	require.NotEmpty(t, c.ParentID, "commit must have a parent to compare against")
	parent, err := env.cs.GetCommit(ctx, c.ParentID)
	require.NoError(t, err)
	return c.TreeHash, parent.TreeHash
}

func TestCommitService_CreateCommit_NothingStaged_ReturnsErrNothingToCommit(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	// A modified-but-unstaged file is the exact shape of the MGIT-77 report:
	// the user sees pending work, and commit must not pretend to record it.
	require.NoError(t, os.WriteFile(filepath.Join(env.repo.Root(), "unstaged.txt"),
		[]byte("work the agent believes it checkpointed\n"), 0o600))

	before, err := env.cs.ListCommits(ctx)
	require.NoError(t, err)

	_, err = env.commit.CreateCommit(ctx, CreateCommitRequest{
		TaskID:  "MGIT-77",
		AgentID: "agent-01",
		Message: "nothing staged",
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrNothingToCommit)

	after, err := env.cs.ListCommits(ctx)
	require.NoError(t, err)
	assert.Len(t, after, len(before), "a refused commit must not advance the branch")
}

func TestCommitService_CreateCommit_NothingStagedAllowEmpty_Succeeds(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	c, err := env.commit.CreateCommit(ctx, CreateCommitRequest{
		TaskID:     "MGIT-77",
		AgentID:    "agent-01",
		Message:    "deliberate empty checkpoint",
		AllowEmpty: true,
	})
	require.NoError(t, err)
	require.NotEmpty(t, c.CommitID)

	tree, parentTree := headAndParentTree(t, env, c.CommitID)
	assert.Equal(t, parentTree, tree,
		"--allow-empty keeps its documented meaning: the tree is unchanged")
}

func TestCommitService_CreateCommit_StagedFile_TreeDiffersFromParent(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	stageFile(t, env, "recorded.txt", "content that must survive the commit\n")

	c, err := env.commit.CreateCommit(ctx, CreateCommitRequest{
		TaskID:  "MGIT-77",
		AgentID: "agent-01",
		Message: "record staged work",
	})
	require.NoError(t, err)

	tree, parentTree := headAndParentTree(t, env, c.CommitID)
	assert.NotEqual(t, parentTree, tree,
		"the commit tree must differ from its parent — otherwise nothing was recorded")

	content, err := env.cs.GetFileFromCommit(ctx, c.CommitID, "recorded.txt")
	require.NoError(t, err, "the staged file must be present in the commit tree")
	assert.Equal(t, "content that must survive the commit\n", string(content))
}

// A staged path whose content is byte-identical to HEAD changes no tree, so it
// is still an empty commit — staging alone is not proof of a change.
func TestCommitService_CreateCommit_StagedButUnchangedContent_ReturnsErrNothingToCommit(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	stageFile(t, env, "same.txt", "identical\n")
	_, err := env.commit.CreateCommit(ctx, CreateCommitRequest{
		TaskID: "MGIT-77", AgentID: "agent-01", Message: "first",
	})
	require.NoError(t, err)

	// Re-stage the very same bytes.
	stageFile(t, env, "same.txt", "identical\n")
	_, err = env.commit.CreateCommit(ctx, CreateCommitRequest{
		TaskID: "MGIT-77", AgentID: "agent-01", Message: "second",
	})
	assert.ErrorIs(t, err, model.ErrNothingToCommit)
}

// A staged deletion is a real change and must be committable.
func TestCommitService_CreateCommit_StagedDeletion_Succeeds(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	stageFile(t, env, "doomed.txt", "temporary\n")
	first, err := env.commit.CreateCommit(ctx, CreateCommitRequest{
		TaskID: "MGIT-77", AgentID: "agent-01", Message: "add",
	})
	require.NoError(t, err)

	require.NoError(t, os.Remove(filepath.Join(env.repo.Root(), "doomed.txt")))
	require.NoError(t, env.wt.Add(ctx, "doomed.txt"))

	second, err := env.commit.CreateCommit(ctx, CreateCommitRequest{
		TaskID: "MGIT-77", AgentID: "agent-01", Message: "delete",
	})
	require.NoError(t, err, "a staged deletion is a change and must commit")
	assert.NotEqual(t, first.TreeHash, second.TreeHash)
}
