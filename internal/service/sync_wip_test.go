package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gitstore "github.com/hyper-swe/mgit/internal/store/git"
)

// TestEnsureSynced_StagedTaskWIP_NotAbsorbedIntoBase is the MGIT-56 core: the
// user's mgit-STAGED paths are pending task work, and a status-time resync
// must not absorb their CONTENT into the [mgit-sync] base — otherwise the
// task's first commit has no delta and its net diff (review surface, squash
// content) is silently empty. Unstaged project drift is still absorbed.
// Refs: MGIT-56, ADR-008 §3
func TestEnsureSynced_StagedTaskWIP_NotAbsorbedIntoBase(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	// Pending task work, explicitly staged for the next commit.
	writeProjectFile(t, env, "wip.go", "package wip\n")
	require.NoError(t, env.wt.Add(ctx, "wip.go"))
	// Independent local project drift (unstaged).
	writeProjectFile(t, env, "drift.go", "package drift\n")

	require.NoError(t, newSyncService(env, "git-A", "").EnsureSynced(ctx))

	head, err := env.repo.Head()
	require.NoError(t, err)
	// The base absorbed the drift...
	got, err := env.cs.GetFileFromCommit(ctx, head, "drift.go")
	require.NoError(t, err)
	assert.Equal(t, "package drift\n", string(got))
	// ...but NOT the staged task WIP.
	_, err = env.cs.GetFileFromCommit(ctx, head, "wip.go")
	assert.Error(t, err, "staged task WIP must not be absorbed into the base")
	// The staging selection survives (MGIT-35 H2 behavior unchanged).
	staged, err := env.repo.StagedSnapshot()
	require.NoError(t, err)
	assert.Equal(t, []string{"wip.go"}, staged)
}

// TestEnsureSynced_AddStatusCommitDiff_TaskDiffNonEmpty is the acceptance
// flow from the field report: add -> status(sync) -> commit -> the task's
// diff must show the change. Refs: MGIT-56
func TestEnsureSynced_AddStatusCommitDiff_TaskDiffNonEmpty(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	writeProjectFile(t, env, "feature.go", "package feature\n")
	require.NoError(t, env.wt.Add(ctx, "feature.go"))

	// `mgit status` runs the sync gate before showing status.
	require.NoError(t, newSyncService(env, "git-B", "").EnsureSynced(ctx))

	c, err := env.commit.CreateCommit(ctx, CreateCommitRequest{
		TaskID: "MGIT-11.1", AgentID: "agent-56", Message: "add feature",
	})
	require.NoError(t, err)

	diffs, err := gitstore.NewDiffStore(env.repo).DiffCommits(ctx, c.ParentID, c.CommitID)
	require.NoError(t, err)
	require.Len(t, diffs, 1, "the task's first commit must still carry its delta after a status-time sync")
	assert.Equal(t, "feature.go", diffs[0].Path)
}

// TestEnsureSynced_OnlyStagedWIP_NoBaseCommit: when the only drift IS the
// staged task WIP, the resync must not append a base commit at all.
// Refs: MGIT-56, ADR-008 §6
func TestEnsureSynced_OnlyStagedWIP_NoBaseCommit(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	headBefore, err := env.repo.Head()
	require.NoError(t, err)

	writeProjectFile(t, env, "wip.go", "package wip\n")
	require.NoError(t, env.wt.Add(ctx, "wip.go"))

	require.NoError(t, newSyncService(env, "git-C", "").EnsureSynced(ctx))

	headAfter, err := env.repo.Head()
	require.NoError(t, err)
	assert.Equal(t, headBefore, headAfter, "staged-WIP-only drift must not advance the base")
}
