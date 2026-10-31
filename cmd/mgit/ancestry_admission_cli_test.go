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

// A host commit with its own host-origin ledger row, built on an undelivered
// guest commit, is not a base, a checkout target or a worktree re-add either:
// the history under it carries the retained bytes. Refs: MGIT-323.5
func TestAncestryAdmission_CLI_HostChildOfUndeliveredGuestCommitIsRefused(t *testing.T) {
	root := projectWithGit(t)
	wt := filepath.Join(t.TempDir(), "wt")
	require.NoError(t, runCLI(t, "worktree", "add", wt, "--task-id", "MGIT-323"))
	t.Chdir(wt)
	require.NoError(t, os.WriteFile(filepath.Join(wt, "retained.txt"), []byte("retained guest bytes\n"), 0o600))
	require.NoError(t, runCLI(t, "commit", "-a", "-m", "a step that stays undelivered"))
	short := lastCommitShort(t)

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

	// The host's own step on top gets a host-origin row of its own.
	require.NoError(t, os.WriteFile(filepath.Join(wt, "host.txt"), []byte("host\n"), 0o600))
	require.NoError(t, runCLI(t, "commit", "-a", "-m", "a host step on top"))
	child := lastCommitShort(t)
	require.NotEqual(t, short, child)

	t.Chdir(root)
	fork := filepath.Join(t.TempDir(), "fork")
	err = runCLI(t, "work", fork, "--task-id", "MGIT-323.9", "--base", child)
	require.ErrorContains(t, err, "undelivered")
	require.NotContains(t, err.Error(), "mgit init")
	_, statErr := os.Stat(fork)
	require.True(t, os.IsNotExist(statErr), "a refused base must not create a worktree")

	err = runCLI(t, "checkout", "task/MGIT-323")
	require.ErrorContains(t, err, "undelivered")
	_, statErr = os.Stat(filepath.Join(root, "retained.txt"))
	require.True(t, os.IsNotExist(statErr), "a refused checkout must not materialize the retained bytes")

	require.NoError(t, runCLI(t, "worktree", "remove", wt, "--force"))
	again := filepath.Join(t.TempDir(), "again")
	err = runCLI(t, "worktree", "add", again, "--task-id", "MGIT-323")
	require.ErrorContains(t, err, "undelivered")
	_, statErr = os.Stat(filepath.Join(again, "retained.txt"))
	require.True(t, os.IsNotExist(statErr), "a refused re-add must not materialize the retained bytes")
}
