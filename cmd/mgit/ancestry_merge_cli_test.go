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

// A merge commit is built on both its parents. A retained, undelivered guest
// commit merged into host history with `merge --no-ff` is part of that
// history, and base, checkout and re-add refuse it through the second parent.
// Refs: MGIT-323.5
func TestAncestryAdmission_CLI_MergeOfAnUndeliveredBranchIsRefused(t *testing.T) {
	root := projectWithGit(t)
	wt := filepath.Join(t.TempDir(), "wt")
	require.NoError(t, runCLI(t, "worktree", "add", wt, "--task-id", "MGIT-323"))
	t.Chdir(wt)
	require.NoError(t, os.WriteFile(filepath.Join(wt, "retained.txt"), []byte("retained guest bytes\n"), 0o600))
	require.NoError(t, runCLI(t, "commit", "-a", "-m", "a step that stays undelivered"))

	t.Chdir(root)
	require.NoError(t, os.WriteFile(filepath.Join(root, "hostside.txt"), []byte("host\n"), 0o600))
	require.NoError(t, runCLI(t, "commit", "-a", "--task-id", "HOST-1", "-m", "host side"))
	// The merge is made while the branch is still admissible, as a merge already
	// in a store would have been; merge now refuses an undelivered source.
	require.NoError(t, runCLI(t, "merge", "task/MGIT-323", "--no-ff", "-m", "merge the branch"))

	// The branch's commit is then retained undelivered.
	idx, err := index.New(filepath.Join(root, ".mgit", "index.db"), time.Now)
	require.NoError(t, err)
	rows, err := idx.GetTaskCommits(context.Background(), "MGIT-323")
	require.NoError(t, err)
	require.NoError(t, idx.AppendTaskCommit(context.Background(), index.TaskCommitInsert{
		TaskID: "AUDIT-pending", CommitHash: rows[len(rows)-1].CommitHash, ContentHash: "retained", Origin: index.OriginLand,
	}))
	require.NoError(t, idx.Close())

	fork := filepath.Join(t.TempDir(), "fork")
	err = runCLI(t, "work", fork, "--task-id", "MGIT-323.9", "--base", "HEAD")
	require.ErrorContains(t, err, "undelivered", "a merge is built on the branch it merged")
	_, statErr := os.Stat(fork)
	require.True(t, os.IsNotExist(statErr), "a refused base must not create a worktree")
}
