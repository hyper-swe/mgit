package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
	gitstore "github.com/hyper-swe/mgit/internal/store/git"
)

// newRestoreForEnv builds a RestoreService against the shared test env.
func newRestoreForEnv(env *testEnv) *RestoreService {
	return NewRestoreService(env.repo, env.cs, env.repo.Root())
}

// TestRestoreAll_RecoversWholeCheckpoint is the MGIT-55 core: one command
// returns the entire working tree to a prior checkpoint's state — files
// restored, task-added files removed — without minting a commit.
// Refs: MGIT-55
func TestRestoreAll_RecoversWholeCheckpoint(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	good := stageAndCommitFiles(t, env, "MGIT-10.1", "good state", map[string]string{
		"a.txt": "v1\n",
		"b.txt": "keep\n",
	})
	stageAndCommitFiles(t, env, "MGIT-10.2", "went wrong", map[string]string{"a.txt": "v2\n", "c.txt": "junk\n"})
	removeAndCommit(t, env, "MGIT-10.2", "dropped b", "b.txt")

	res, err := newRestoreForEnv(env).RestoreAll(ctx, good.CommitID, false)
	require.NoError(t, err)
	assert.Equal(t, "restored", res.Status)
	assert.NotZero(t, res.FilesChanged)

	// Whole working tree matches the checkpoint.
	assert.Equal(t, "v1\n", readWorking(t, env, "a.txt"))
	assert.Equal(t, "keep\n", readWorking(t, env, "b.txt"), "deleted file must come back")
	_, statErr := os.Stat(filepath.Join(env.repo.Root(), "c.txt"))
	assert.True(t, os.IsNotExist(statErr), "post-checkpoint file must be removed")

	// No commit minted, nothing staged: the agent reviews and commits.
	commits, err := env.cs.ListCommits(ctx)
	require.NoError(t, err)
	for _, c := range commits {
		assert.NotContains(t, c.Message, "restore --all", "restore must not mint a commit")
	}
}

// TestRestoreAll_ThenCommit_Composes: the recovered state can be staged and
// committed as the task's next step — the salvage loop's mechanical form.
// Refs: MGIT-55
func TestRestoreAll_ThenCommit_Composes(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	good := stageAndCommitFiles(t, env, "MGIT-10.1", "good", map[string]string{"a.txt": "v1\n"})
	stageAndCommitFiles(t, env, "MGIT-10.3", "bad", map[string]string{"a.txt": "v2\n"})

	_, err := newRestoreForEnv(env).RestoreAll(ctx, good.CommitID, false)
	require.NoError(t, err)

	c := stageAndCommitFiles(t, env, "MGIT-10.3", "back to good", map[string]string{"a.txt": "v1\n"})
	got, err := env.cs.GetFileFromCommit(ctx, c.CommitID, "a.txt")
	require.NoError(t, err)
	assert.Equal(t, "v1\n", string(got))
}

// TestRestoreAll_DirtyTree_Refuses: uncommitted changes that the restore
// would overwrite must block it with a clear error. Refs: MGIT-55
func TestRestoreAll_DirtyTree_Refuses(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	good := stageAndCommitFiles(t, env, "MGIT-10.1", "good", map[string]string{"a.txt": "v1\n"})
	stageAndCommitFiles(t, env, "MGIT-10.4", "bad", map[string]string{"a.txt": "v2\n"})

	// Uncommitted local edit on a path the restore would rewrite.
	require.NoError(t, os.WriteFile(filepath.Join(env.repo.Root(), "a.txt"), []byte("in-flight\n"), 0o600))

	_, err := newRestoreForEnv(env).RestoreAll(ctx, good.CommitID, false)
	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrContentConflict)
	assert.Contains(t, err.Error(), "a.txt")
	assert.Equal(t, "in-flight\n", readWorking(t, env, "a.txt"), "dirty file must be untouched")

	// --force performs the recovery over the uncommitted state.
	res, err := newRestoreForEnv(env).RestoreAll(ctx, good.CommitID, true)
	require.NoError(t, err)
	assert.Equal(t, "restored", res.Status)
	assert.Equal(t, "v1\n", readWorking(t, env, "a.txt"))
}

// TestRestoreAll_TrashedUncommittedTree_ForcedRecovery is the checkpoint
// recovery scenario itself: the tree was trashed AFTER the last commit (so
// HEAD's tree equals the target), and restore --all --force must still
// recover it — the first implementation diffed committed trees and no-opped
// here. Refs: MGIT-55 (review finding M1)
func TestRestoreAll_TrashedUncommittedTree_ForcedRecovery(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	head := stageAndCommitFiles(t, env, "MGIT-10.6", "checkpoint", map[string]string{"a.txt": "good\n"})

	// Trash the tree WITHOUT committing.
	require.NoError(t, os.WriteFile(filepath.Join(env.repo.Root(), "a.txt"), []byte("TRASHED\n"), 0o600))

	// Unforced: refuses (the trash is uncommitted state).
	_, err := newRestoreForEnv(env).RestoreAll(ctx, head.CommitID, false)
	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrContentConflict)

	// Forced: recovers, and must NOT report "unchanged".
	res, err := newRestoreForEnv(env).RestoreAll(ctx, head.CommitID, true)
	require.NoError(t, err)
	assert.Equal(t, "restored", res.Status)
	assert.Equal(t, "good\n", readWorking(t, env, "a.txt"))
}

// TestRestoreAll_StagesRestoredPaths_SyncDoesNotAbsorb: the restored paths
// are staged, so a status-time resync treats them as task WIP (MGIT-56) and
// the follow-up task commit keeps its diff. Refs: MGIT-55 (review finding M2)
func TestRestoreAll_StagesRestoredPaths_SyncDoesNotAbsorb(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	good := stageAndCommitFiles(t, env, "MGIT-10.7", "good", map[string]string{"a.txt": "v1\n"})
	stageAndCommitFiles(t, env, "MGIT-10.8", "bad", map[string]string{"a.txt": "v2\n"})

	_, err := newRestoreForEnv(env).RestoreAll(ctx, good.CommitID, false)
	require.NoError(t, err)

	// The restored path is staged...
	staged, err := env.repo.StagedSnapshot()
	require.NoError(t, err)
	assert.Contains(t, staged, "a.txt", "restored paths must be staged (MGIT-56 protection)")

	// ...so the status-time resync does NOT absorb it, and the follow-up
	// commit carries the restoration as the task's step.
	require.NoError(t, newSyncService(env, "git-R", "").EnsureSynced(ctx))
	c, err := env.commit.CreateCommit(ctx, CreateCommitRequest{
		TaskID: "MGIT-10.8", AgentID: "agent-55", Message: "recover good state",
	})
	require.NoError(t, err)
	diffs, err := gitstore.NewDiffStore(env.repo).DiffCommits(ctx, c.ParentID, c.CommitID)
	require.NoError(t, err)
	require.NotEmpty(t, diffs, "the restoration must land as the task's own delta, not a sync commit")
}

// TestRestoreAll_AlreadyAtCheckpoint_NoOp: restoring to the current state is
// an idempotent no-op, not an error. Refs: MGIT-55
func TestRestoreAll_AlreadyAtCheckpoint_NoOp(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	head := stageAndCommitFiles(t, env, "MGIT-10.1", "state", map[string]string{"a.txt": "v1\n"})

	res, err := newRestoreForEnv(env).RestoreAll(ctx, head.CommitID, false)
	require.NoError(t, err)
	assert.Equal(t, "unchanged", res.Status)
	assert.Zero(t, res.FilesChanged)
}

// TestRestoreAll_UnknownCommit_Error. Refs: MGIT-55
func TestRestoreAll_UnknownCommit_Error(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()
	stageAndCommitFiles(t, env, "MGIT-10.1", "state", map[string]string{"a.txt": "v1\n"})

	_, err := newRestoreForEnv(env).RestoreAll(ctx, "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", false)
	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrCommitNotFound)
}
