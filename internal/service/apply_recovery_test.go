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

// TestRecoverPendingApply_CompletesInterruptedApply simulates a crash between
// the ref advance and the working-directory/index writes: the tip carries the
// revert tree, disk still has the pre-revert content, and the journal is
// pending. Recovery must materialize the diffs, index the commit, and clear
// the journal — closing the window where the auto-resync would absorb the
// stale disk and silently undo the revert. Refs: MGIT-54 (review finding H3)
func TestRecoverPendingApply_CompletesInterruptedApply(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	stageAndCommitFiles(t, env, "MGIT-13.1", "good", map[string]string{"f.txt": "good\n"})
	bad := stageAndCommitFiles(t, env, "MGIT-13.2", "bad", map[string]string{"f.txt": "bad\n"})

	// The inverse of the bad task, advanced onto the tip WITHOUT materializing
	// (the plain, unjournaled primitive) — then a journal as the crashed
	// process would have left it.
	inverse, err := gitstore.NewDiffStore(env.repo).DiffCommits(ctx, bad.CommitID, bad.ParentID)
	require.NoError(t, err)
	revert := &model.Commit{Message: "[MGIT:MGIT-13.2] Revert: crash sim (1 commits)", AgentID: "mgit-rollback"}
	hash, err := env.cs.CreateCommitFromDiffs(ctx, revert, inverse)
	require.NoError(t, err)
	require.NoError(t, env.repo.WriteApplyJournal(gitstore.ApplyJournal{
		Root: env.repo.Root(), CommitHash: hash, ContentHash: revert.ContentHash,
		Index: gitstore.ApplyIndexEntry{TaskID: "MGIT-13.2", AgentID: "mgit-rollback"},
		Diffs: inverse,
	}))
	// Crash state: tip = revert tree, disk = pre-revert.
	assert.Equal(t, "bad\n", readWorking(t, env, "f.txt"))

	require.NoError(t, RecoverPendingApply(ctx, env.repo, env.cs, env.idx))

	// Disk completed, commit indexed, journal gone.
	assert.Equal(t, "good\n", readWorking(t, env, "f.txt"), "recovery must complete the materialization")
	records, err := env.idx.GetTaskCommits(ctx, "MGIT-13.2")
	require.NoError(t, err)
	var indexed bool
	for _, r := range records {
		if r.CommitHash == hash {
			indexed = true
		}
	}
	assert.True(t, indexed, "recovery must index the applied commit")
	_, found, err := env.repo.ReadApplyJournal()
	require.NoError(t, err)
	assert.False(t, found, "journal must be cleared after recovery")

	// Recovery is idempotent.
	require.NoError(t, RecoverPendingApply(ctx, env.repo, env.cs, env.idx))
}

// TestRecoverPendingApply_RefNeverAdvanced_ClearsJournal: a crash BEFORE the
// ref advance leaves a journal pointing at a commit that never became the
// tip; recovery must discard it without touching disk. Refs: MGIT-54 (H3)
func TestRecoverPendingApply_RefNeverAdvanced_ClearsJournal(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	stageAndCommitFiles(t, env, "MGIT-13.3", "state", map[string]string{"f.txt": "current\n"})

	// A commit on a side branch stands in for the never-referenced commit
	// (not on the current lineage).
	_, err := env.branch.CreateNamedBranch(ctx, "side")
	require.NoError(t, err)
	require.NoError(t, env.branch.SwitchBranch(ctx, "side"))
	side := stageAndCommitFiles(t, env, "MGIT-13.4", "side work", map[string]string{"side.txt": "s\n"})
	require.NoError(t, env.branch.SwitchBranch(ctx, "main"))
	// Remove side.txt from disk (it belongs to the side branch state).
	require.NoError(t, os.Remove(filepath.Join(env.repo.Root(), "side.txt")))

	require.NoError(t, env.repo.WriteApplyJournal(gitstore.ApplyJournal{
		Root: env.repo.Root(), CommitHash: side.CommitID, ContentHash: side.ContentHash,
		Index: gitstore.ApplyIndexEntry{TaskID: "MGIT-13.4", AgentID: "mgit-rollback"},
		Diffs: []model.FileDiff{{Path: "side.txt", Operation: model.DiffAdded, NewHash: "irrelevant"}},
	}))

	require.NoError(t, RecoverPendingApply(ctx, env.repo, env.cs, env.idx))

	_, found, err := env.repo.ReadApplyJournal()
	require.NoError(t, err)
	assert.False(t, found, "journal for a never-landed commit must be discarded")
	assert.Equal(t, "current\n", readWorking(t, env, "f.txt"), "disk untouched")
	_, statErr := os.Stat(filepath.Join(env.repo.Root(), "side.txt"))
	assert.True(t, os.IsNotExist(statErr), "discarded journal must not materialize anything")
}

// TestRecoverPendingApply_ForeignRoot_LeftPending: a journal recorded by a
// different root (another worktree) is left for that root's next open, and
// new applies from this root refuse to overwrite it. Refs: MGIT-54 (H3)
func TestRecoverPendingApply_ForeignRoot_LeftPending(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	stageAndCommitFiles(t, env, "MGIT-13.5", "state", map[string]string{"f.txt": "v\n"})
	require.NoError(t, env.repo.WriteApplyJournal(gitstore.ApplyJournal{
		Root: "/somewhere/else", CommitHash: "0000000000000000000000000000000000000000",
		Index: gitstore.ApplyIndexEntry{TaskID: "MGIT-13.5", AgentID: "x"},
	}))

	require.NoError(t, RecoverPendingApply(ctx, env.repo, env.cs, env.idx))
	_, found, err := env.repo.ReadApplyJournal()
	require.NoError(t, err)
	assert.True(t, found, "a foreign root's journal must be left pending")

	// And a new journaled write from this root must refuse.
	err = env.repo.WriteApplyJournal(gitstore.ApplyJournal{Root: env.repo.Root()})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "awaits recovery")
}

// TestRollbackTask_ClearsJournal: the happy path leaves no journal behind.
// Refs: MGIT-54 (H3)
func TestRollbackTask_ClearsJournal(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	stageAndCommitFiles(t, env, "MGIT-13.6", "good", map[string]string{"f.txt": "good\n"})
	stageAndCommitFiles(t, env, "MGIT-13.7", "bad", map[string]string{"f.txt": "bad\n"})

	_, err := env.rollbk.RollbackTask(ctx, RollbackRequest{TaskID: "MGIT-13.7"})
	require.NoError(t, err)
	_, found, err := env.repo.ReadApplyJournal()
	require.NoError(t, err)
	assert.False(t, found)
}
