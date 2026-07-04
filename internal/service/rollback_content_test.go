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

// stageAndCommitFiles writes files into the repo root, stages them, and commits
// them under taskID through the real CommitService, returning the commit.
func stageAndCommitFiles(t *testing.T, env *testEnv, taskID, msg string, files map[string]string) *model.Commit {
	t.Helper()
	ctx := context.Background()
	for rel, content := range files {
		p := filepath.Join(env.repo.Root(), rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o750))
		require.NoError(t, os.WriteFile(p, []byte(content), 0o600))
	}
	for rel := range files {
		require.NoError(t, env.wt.Add(ctx, rel))
	}
	c, err := env.commit.CreateCommit(ctx, CreateCommitRequest{
		TaskID: taskID, AgentID: "agent-cc", Message: msg,
	})
	require.NoError(t, err)
	return c
}

// removeAndCommit deletes a file, stages the deletion, and commits it.
func removeAndCommit(t *testing.T, env *testEnv, taskID, msg, rel string) *model.Commit {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, os.Remove(filepath.Join(env.repo.Root(), rel)))
	require.NoError(t, env.wt.Add(ctx, rel))
	c, err := env.commit.CreateCommit(ctx, CreateCommitRequest{
		TaskID: taskID, AgentID: "agent-cc", Message: msg,
	})
	require.NoError(t, err)
	return c
}

// readWorking reads a working-directory file's bytes.
func readWorking(t *testing.T, env *testEnv, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(env.repo.Root(), rel)) //nolint:gosec // test path
	require.NoError(t, err)
	return string(b)
}

// TestRollbackTask_RestoresContent_TreeAndWorktree is the MGIT-54 core: a
// rollback must actually restore the pre-task state — in the new commit's
// tree AND on disk — not just record intent. Refs: MGIT-54, FR-6
func TestRollbackTask_RestoresContent_TreeAndWorktree(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	stageAndCommitFiles(t, env, "MGIT-9.1", "good base", map[string]string{"file.txt": "good\n"})
	bad := stageAndCommitFiles(t, env, "MGIT-9.2", "bad change", map[string]string{
		"file.txt": "bad\n",
		"junk.txt": "junk\n",
	})

	revert, err := env.rollbk.RollbackTask(ctx, RollbackRequest{TaskID: "MGIT-9.2", Reason: "wrong lib"})
	require.NoError(t, err)

	// Working directory restored.
	assert.Equal(t, "good\n", readWorking(t, env, "file.txt"), "working file must be restored")
	_, statErr := os.Stat(filepath.Join(env.repo.Root(), "junk.txt"))
	assert.True(t, os.IsNotExist(statErr), "file added by the task must be removed from disk")

	// Revert commit tree restored.
	got, err := env.cs.GetFileFromCommit(ctx, revert.CommitID, "file.txt")
	require.NoError(t, err)
	assert.Equal(t, "good\n", string(got), "revert commit tree must carry restored bytes")
	_, err = env.cs.GetFileFromCommit(ctx, revert.CommitID, "junk.txt")
	assert.Error(t, err, "task-added file must be absent from the revert tree")

	// Append-only: the bad commit remains retrievable.
	_, err = env.cs.GetCommit(ctx, bad.CommitID)
	assert.NoError(t, err)
}

// TestRollbackTask_MultiCommitTask_NetRevert: a task with several commits is
// reverted by its NET change (intermediate states are not replayed).
// Refs: MGIT-54
func TestRollbackTask_MultiCommitTask_NetRevert(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	stageAndCommitFiles(t, env, "MGIT-9.1", "base", map[string]string{"a.txt": "v1\n"})
	stageAndCommitFiles(t, env, "MGIT-9.3", "step one", map[string]string{"a.txt": "v2\n"})
	stageAndCommitFiles(t, env, "MGIT-9.3", "step two", map[string]string{"a.txt": "v3\n", "b.txt": "new\n"})

	revert, err := env.rollbk.RollbackTask(ctx, RollbackRequest{TaskID: "MGIT-9.3"})
	require.NoError(t, err)

	assert.Equal(t, "v1\n", readWorking(t, env, "a.txt"))
	got, err := env.cs.GetFileFromCommit(ctx, revert.CommitID, "a.txt")
	require.NoError(t, err)
	assert.Equal(t, "v1\n", string(got))
	_, err = env.cs.GetFileFromCommit(ctx, revert.CommitID, "b.txt")
	assert.Error(t, err, "b.txt was added by the task and must be gone")
}

// TestRollbackTask_DirtyWorktree_Refuses: uncommitted edits on an affected
// path must block the rollback with ErrRollbackConflict — never clobber the
// agent's in-flight work. Refs: MGIT-54
func TestRollbackTask_DirtyWorktree_Refuses(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	stageAndCommitFiles(t, env, "MGIT-9.1", "base", map[string]string{"file.txt": "good\n"})
	stageAndCommitFiles(t, env, "MGIT-9.4", "bad", map[string]string{"file.txt": "bad\n"})

	// Uncommitted local edit on the affected path.
	require.NoError(t, os.WriteFile(filepath.Join(env.repo.Root(), "file.txt"), []byte("in-flight\n"), 0o600))

	_, err := env.rollbk.RollbackTask(ctx, RollbackRequest{TaskID: "MGIT-9.4"})
	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrRollbackConflict)
	assert.Contains(t, err.Error(), "file.txt")
	// The in-flight edit is untouched.
	assert.Equal(t, "in-flight\n", readWorking(t, env, "file.txt"))
}

// TestRollbackTask_AfterExternalChange_Conflict: if another task changed the
// same path after this task, rolling back would clobber that work — refuse
// with a conflict. Refs: MGIT-54
func TestRollbackTask_AfterExternalChange_Conflict(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	stageAndCommitFiles(t, env, "MGIT-9.1", "base", map[string]string{"file.txt": "one\n"})
	stageAndCommitFiles(t, env, "MGIT-9.5", "target task", map[string]string{"file.txt": "two\n"})
	stageAndCommitFiles(t, env, "MGIT-9.6", "later task", map[string]string{"file.txt": "three\n"})

	_, err := env.rollbk.RollbackTask(ctx, RollbackRequest{TaskID: "MGIT-9.5"})
	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrRollbackConflict)
	assert.ErrorIs(t, err, model.ErrContentConflict)
	// Nothing changed.
	assert.Equal(t, "three\n", readWorking(t, env, "file.txt"))
}

// TestRollbackTask_NetEmpty_NoOp: a task whose commits cancel out (add then
// delete) has nothing to revert; no commit is minted. Refs: MGIT-54
func TestRollbackTask_NetEmpty_NoOp(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	stageAndCommitFiles(t, env, "MGIT-9.1", "base", map[string]string{"keep.txt": "k\n"})
	stageAndCommitFiles(t, env, "MGIT-9.7", "add tmp", map[string]string{"tmp.txt": "t\n"})
	removeAndCommit(t, env, "MGIT-9.7", "drop tmp", "tmp.txt")

	before, err := env.cs.ListCommits(ctx)
	require.NoError(t, err)

	_, err = env.rollbk.RollbackTask(ctx, RollbackRequest{TaskID: "MGIT-9.7"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no net content or mode change")

	after, err := env.cs.ListCommits(ctx)
	require.NoError(t, err)
	assert.Len(t, after, len(before), "no commit may be minted for a net-empty rollback")
}

// TestRollbackTask_SecondRollback_NetEmpty: after a successful rollback the
// task's net change (including the revert) is zero, so a second rollback is a
// clean no-op error instead of re-applying the task. Refs: MGIT-54
func TestRollbackTask_SecondRollback_NetEmpty(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	stageAndCommitFiles(t, env, "MGIT-9.1", "base", map[string]string{"file.txt": "good\n"})
	stageAndCommitFiles(t, env, "MGIT-9.8", "bad", map[string]string{"file.txt": "bad\n"})

	_, err := env.rollbk.RollbackTask(ctx, RollbackRequest{TaskID: "MGIT-9.8"})
	require.NoError(t, err)
	assert.Equal(t, "good\n", readWorking(t, env, "file.txt"))

	_, err = env.rollbk.RollbackTask(ctx, RollbackRequest{TaskID: "MGIT-9.8"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no net content or mode change")
	assert.Equal(t, "good\n", readWorking(t, env, "file.txt"))
}

// TestRollbackTask_DryRun_NoChanges: dry run reports the inverse diffs but
// mutates nothing — no commit, no disk writes. Refs: MGIT-54, FR-6
func TestRollbackTask_DryRun_NoChanges(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	stageAndCommitFiles(t, env, "MGIT-9.1", "base", map[string]string{"file.txt": "good\n"})
	stageAndCommitFiles(t, env, "MGIT-9.9", "bad", map[string]string{"file.txt": "bad\n"})

	rc, err := env.rollbk.RollbackTask(ctx, RollbackRequest{TaskID: "MGIT-9.9", DryRun: true})
	require.NoError(t, err)
	assert.NotEmpty(t, rc.FileDiffs, "dry run must report the inverse diffs")
	assert.Equal(t, "bad\n", readWorking(t, env, "file.txt"), "dry run must not touch the working dir")
	assert.Empty(t, rc.CommitID, "dry run must not mint a commit")
}

// TestRollbackTask_InterleavedOtherTaskCommit_Conflict is the review's H1
// repro: another task's commit lands BETWEEN the target task's commits on
// the same path. Netting first/last state would silently destroy the
// interleaved work; the fold's chain verification must refuse instead.
// Refs: MGIT-54 (review finding H1)
func TestRollbackTask_InterleavedOtherTaskCommit_Conflict(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	stageAndCommitFiles(t, env, "MGIT-9.10", "base", map[string]string{"f.txt": "v1\n"})
	stageAndCommitFiles(t, env, "MGIT-9.11", "A step 1", map[string]string{"f.txt": "v2\n"})
	stageAndCommitFiles(t, env, "MGIT-9.12", "B interleaved", map[string]string{"f.txt": "v3-from-B\n"})
	stageAndCommitFiles(t, env, "MGIT-9.11", "A step 2", map[string]string{"f.txt": "v4\n"})

	_, err := env.rollbk.RollbackTask(ctx, RollbackRequest{TaskID: "MGIT-9.11"})
	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrRollbackConflict)
	assert.Contains(t, err.Error(), "f.txt")
	assert.Equal(t, "v4\n", readWorking(t, env, "f.txt"), "nothing may change on a chain conflict")
}

// TestRollbackTask_TypeChange_RestoresOriginalType is the review's H2 repro:
// a task converts a regular file into a symlink; rollback must restore a
// REGULAR file with the original bytes, not a symlink whose target is the
// old content. Refs: MGIT-54 (review finding H2)
func TestRollbackTask_TypeChange_RestoresOriginalType(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	stageAndCommitFiles(t, env, "MGIT-9.13", "regular file", map[string]string{"f.txt": "original content\n"})

	// The task replaces it with a symlink.
	require.NoError(t, os.Remove(filepath.Join(env.repo.Root(), "f.txt")))
	require.NoError(t, os.Symlink("target.txt", filepath.Join(env.repo.Root(), "f.txt")))
	require.NoError(t, env.wt.Add(ctx, "f.txt"))
	_, err := env.commit.CreateCommit(ctx, CreateCommitRequest{
		TaskID: "MGIT-9.14", AgentID: "agent-cc", Message: "convert to symlink",
	})
	require.NoError(t, err)

	_, err = env.rollbk.RollbackTask(ctx, RollbackRequest{TaskID: "MGIT-9.14"})
	require.NoError(t, err)

	info, err := os.Lstat(filepath.Join(env.repo.Root(), "f.txt"))
	require.NoError(t, err)
	assert.Zero(t, info.Mode()&os.ModeSymlink, "restored entry must be a REGULAR file, not a symlink")
	assert.Equal(t, "original content\n", readWorking(t, env, "f.txt"))
}

// TestRollbackTask_ModeOnlyChange_Reverted is the review's M4 repro: a task
// that only chmods a file must be revertible (mode is state too).
// Refs: MGIT-54 (review finding M4)
func TestRollbackTask_ModeOnlyChange_Reverted(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	stageAndCommitFiles(t, env, "MGIT-9.15", "script", map[string]string{"run.sh": "#!/bin/sh\n"})

	// The task's only change: make it executable.
	require.NoError(t, os.Chmod(filepath.Join(env.repo.Root(), "run.sh"), 0o700)) //nolint:gosec // executable fixture is the point
	require.NoError(t, env.wt.Add(ctx, "run.sh"))
	_, err := env.commit.CreateCommit(ctx, CreateCommitRequest{
		TaskID: "MGIT-9.16", AgentID: "agent-cc", Message: "chmod +x",
	})
	require.NoError(t, err)

	_, err = env.rollbk.RollbackTask(ctx, RollbackRequest{TaskID: "MGIT-9.16"})
	require.NoError(t, err, "a mode-only change is a real change and must be revertible")

	info, err := os.Stat(filepath.Join(env.repo.Root(), "run.sh"))
	require.NoError(t, err)
	assert.Zero(t, info.Mode()&0o100, "executable bit must be reverted")
}
