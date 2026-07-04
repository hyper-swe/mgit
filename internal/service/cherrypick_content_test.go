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

// TestCherryPick_MaterializesContent is the MGIT-54 core for cherry-pick: the
// picked commit's changes must land in the new commit's tree AND on disk, not
// just as a provenance record. Refs: MGIT-54
func TestCherryPick_MaterializesContent(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	stageAndCommitFiles(t, env, "MGIT-7.1", "base", map[string]string{"a.txt": "a\n"})
	pick := stageAndCommitFiles(t, env, "MGIT-7.2", "add util", map[string]string{"util.txt": "useful\n"})
	removeAndCommit(t, env, "MGIT-7.3", "drop util", "util.txt")

	// Sanity: the file is gone before the pick.
	_, statErr := os.Stat(filepath.Join(env.repo.Root(), "util.txt"))
	require.True(t, os.IsNotExist(statErr))

	picked, err := env.commit.CherryPick(ctx, CherryPickRequest{SourceHash: pick.CommitID})
	require.NoError(t, err)

	// On disk and in the new commit's tree.
	assert.Equal(t, "useful\n", readWorking(t, env, "util.txt"), "picked content must be materialized")
	got, err := env.cs.GetFileFromCommit(ctx, picked.CommitID, "util.txt")
	require.NoError(t, err)
	assert.Equal(t, "useful\n", string(got))
	// Provenance preserved in the message.
	assert.Contains(t, picked.Message, "cherry-pick")
}

// TestCherryPick_TaskDerivation: the new commit's task defaults to the source
// commit's provenance and can be overridden. Refs: MGIT-54, MGIT-19
func TestCherryPick_TaskDerivation(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	stageAndCommitFiles(t, env, "MGIT-7.1", "base", map[string]string{"a.txt": "a\n"})
	pick := stageAndCommitFiles(t, env, "MGIT-7.2", "add util", map[string]string{"util.txt": "u\n"})
	removeAndCommit(t, env, "MGIT-7.3", "drop util", "util.txt")

	// Default: derived from the source.
	picked, err := env.commit.CherryPick(ctx, CherryPickRequest{SourceHash: pick.CommitID})
	require.NoError(t, err)
	assert.Equal(t, "MGIT-7.2", picked.TaskID.String())

	// Indexed under that task.
	records, err := env.idx.GetTaskCommits(ctx, "MGIT-7.2")
	require.NoError(t, err)
	require.Len(t, records, 2, "source + pick both indexed under the task")

	// Override: the pick can be re-attributed. (Drop the file again first so
	// the add applies cleanly.)
	removeAndCommit(t, env, "MGIT-7.4", "drop util again", "util.txt")
	picked2, err := env.commit.CherryPick(ctx, CherryPickRequest{SourceHash: pick.CommitID, TaskID: "MGIT-7.5"})
	require.NoError(t, err)
	assert.Equal(t, "MGIT-7.5", picked2.TaskID.String())
}

// TestCherryPick_Conflict_PathDiverged: picking a change whose old state no
// longer matches the current tree must refuse with ErrContentConflict.
// Refs: MGIT-54
func TestCherryPick_Conflict_PathDiverged(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	stageAndCommitFiles(t, env, "MGIT-7.1", "base", map[string]string{"a.txt": "v1\n"})
	pick := stageAndCommitFiles(t, env, "MGIT-7.2", "to v2", map[string]string{"a.txt": "v2\n"})
	stageAndCommitFiles(t, env, "MGIT-7.3", "to v3", map[string]string{"a.txt": "v3\n"})

	// Re-picking the v1->v2 change onto a tree at v3 must conflict, since
	// the pick's recorded old state (v1) is gone.
	_, err := env.commit.CherryPick(ctx, CherryPickRequest{SourceHash: pick.CommitID})
	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrContentConflict)
	assert.Equal(t, "v3\n", readWorking(t, env, "a.txt"), "conflict must leave the tree untouched")
}

// TestCherryPick_DirtyPath_Refuses: uncommitted local changes on an affected
// path block the pick. Refs: MGIT-54
func TestCherryPick_DirtyPath_Refuses(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	stageAndCommitFiles(t, env, "MGIT-7.1", "base", map[string]string{"a.txt": "a\n"})
	pick := stageAndCommitFiles(t, env, "MGIT-7.2", "add util", map[string]string{"util.txt": "u\n"})
	removeAndCommit(t, env, "MGIT-7.3", "drop util", "util.txt")

	// An untracked file now occupies the pick's target path.
	require.NoError(t, os.WriteFile(filepath.Join(env.repo.Root(), "util.txt"), []byte("local\n"), 0o600))

	_, err := env.commit.CherryPick(ctx, CherryPickRequest{SourceHash: pick.CommitID})
	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrContentConflict)
	assert.Equal(t, "local\n", readWorking(t, env, "util.txt"), "local file must be untouched")
}

// TestCherryPick_NoNetChange_Error: picking a commit with no tree change is a
// clear error, not an empty commit. Refs: MGIT-54
func TestCherryPick_NoNetChange_Error(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	stageAndCommitFiles(t, env, "MGIT-7.1", "base", map[string]string{"a.txt": "a\n"})
	// A message-only commit (nothing staged) has an unchanged tree.
	empty, err := env.commit.CreateCommit(ctx, CreateCommitRequest{
		TaskID: "MGIT-7.6", AgentID: "agent-cc", Message: "note only",
	})
	require.NoError(t, err)

	_, err = env.commit.CherryPick(ctx, CherryPickRequest{SourceHash: empty.CommitID})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no content change")
}
