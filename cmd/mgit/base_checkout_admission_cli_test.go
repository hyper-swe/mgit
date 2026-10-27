package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hyper-swe/mgit/internal/store/index"
	"github.com/stretchr/testify/require"
)

// A retained guest commit that was never delivered is not a base for a new
// task and not a checkout target, through the real command tree. Refs: MGIT-323.2, MGIT-323.3
func TestBaseAdmission_CLI_UndeliveredGuestCommitIsNotABaseOrCheckoutTarget(t *testing.T) {
	root := projectWithGit(t)
	wt := filepath.Join(t.TempDir(), "wt")
	require.NoError(t, runCLI(t, "worktree", "add", wt, "--task-id", "MGIT-323"))
	t.Chdir(wt)
	require.NoError(t, os.WriteFile(filepath.Join(wt, "retained.txt"), []byte("retained guest bytes\n"), 0o600))
	require.NoError(t, runCLI(t, "commit", "-a", "-m", "a step that stays undelivered"))
	short := lastCommitShort(t)

	// Mark that commit as a retained, undelivered guest land row.
	idx, err := index.New(filepath.Join(root, ".mgit", "index.db"), time.Now)
	require.NoError(t, err)
	rows, err := idx.GetTaskCommits(context.Background(), "MGIT-323")
	require.NoError(t, err)
	full := rows[len(rows)-1].CommitHash
	require.Contains(t, full, short)
	require.NoError(t, idx.AppendTaskCommit(context.Background(), index.TaskCommitInsert{
		TaskID: "AUDIT-pending", CommitHash: full, ContentHash: "retained", Origin: index.OriginLand,
	}))
	require.NoError(t, idx.Close())

	t.Chdir(root)
	fork := filepath.Join(t.TempDir(), "fork")
	err = runCLI(t, "work", fork, "--task-id", "MGIT-323.9", "--base", short)
	require.ErrorContains(t, err, "undelivered")
	require.ErrorContains(t, err, "choose a delivered base")
	require.NotContains(t, err.Error(), "mgit init", "a base choice is not the repository recovery")
	_, statErr := os.Stat(fork)
	require.True(t, os.IsNotExist(statErr), "a refused base must not create a worktree")

	err = runCLI(t, "checkout", "task/MGIT-323")
	require.ErrorContains(t, err, "undelivered")
	require.ErrorContains(t, err, "stay on the current branch")
	require.NotContains(t, err.Error(), "mgit init", "a checkout choice is not the repository recovery")
	_, statErr = os.Stat(filepath.Join(root, "retained.txt"))
	require.True(t, os.IsNotExist(statErr), "a refused checkout must not materialize the retained bytes")

	// Re-provisioning the task's own worktree is the same property: the task
	// branch ends at the retained commit, so the bytes must not reach disk.
	require.NoError(t, runCLI(t, "worktree", "remove", wt, "--force"))
	again := filepath.Join(t.TempDir(), "again")
	err = runCLI(t, "worktree", "add", again, "--task-id", "MGIT-323")
	require.ErrorContains(t, err, "undelivered")
	require.NotContains(t, err.Error(), "mgit init")
	_, statErr = os.Stat(filepath.Join(again, "retained.txt"))
	require.True(t, os.IsNotExist(statErr), "a refused re-add must not materialize the retained bytes")

	// Accepted history keeps working from the same root.
	ok := filepath.Join(t.TempDir(), "ok")
	require.NoError(t, runCLI(t, "work", ok, "--task-id", "MGIT-323.8", "--base", "HEAD"))
}
