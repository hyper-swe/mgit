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

// undeliveredTaskBranch makes task/MGIT-323 end at a retained guest commit
// whose ledger row is sandbox-land and not delivered, and returns the project
// root. withHostChild adds a host commit on top of it on the same branch.
func undeliveredTaskBranch(t *testing.T, withHostChild bool) string {
	t.Helper()
	root := projectWithGit(t)
	wt := filepath.Join(t.TempDir(), "wt")
	require.NoError(t, runCLI(t, "worktree", "add", wt, "--task-id", "MGIT-323"))
	t.Chdir(wt)
	require.NoError(t, os.WriteFile(filepath.Join(wt, "retained.txt"), []byte("retained guest bytes\n"), 0o600))
	require.NoError(t, runCLI(t, "commit", "-a", "-m", "a step that stays undelivered"))

	idx, err := index.New(filepath.Join(root, ".mgit", "index.db"), time.Now)
	require.NoError(t, err)
	rows, err := idx.GetTaskCommits(context.Background(), "MGIT-323")
	require.NoError(t, err)
	require.NoError(t, idx.AppendTaskCommit(context.Background(), index.TaskCommitInsert{
		TaskID: "AUDIT-pending", CommitHash: rows[len(rows)-1].CommitHash, ContentHash: "retained", Origin: index.OriginLand,
	}))
	require.NoError(t, idx.Close())

	if withHostChild {
		require.NoError(t, os.WriteFile(filepath.Join(wt, "host.txt"), []byte("host\n"), 0o600))
		require.NoError(t, runCLI(t, "commit", "-a", "-m", "a host step on top"))
	}
	t.Chdir(root)
	return root
}

// A merge integrates the source branch's bytes into the host branch and disk.
// A source that ends at, or is built on, a retained undelivered guest commit is
// refused for every strategy, before any ref or file changes. Refs: MGIT-323.4
func TestMergeAdmission_CLI_RefusesUndeliveredSourceForEveryStrategy(t *testing.T) {
	for name, tc := range map[string]struct {
		args      []string
		hostChild bool
	}{
		"fast-forward":             {nil, false},
		"no-ff":                    {[]string{"--no-ff"}, false},
		"squash":                   {[]string{"--squash"}, false},
		"host child, fast-forward": {nil, true},
		"host child, no-ff":        {[]string{"--no-ff"}, true},
	} {
		t.Run(name, func(t *testing.T) {
			root := undeliveredTaskBranch(t, tc.hostChild)
			before := lastCommitShort(t)

			err := runCLI(t, append([]string{"merge", "task/MGIT-323"}, tc.args...)...)
			require.ErrorContains(t, err, "undelivered")
			require.Equal(t, before, lastCommitShort(t), "a refused merge must leave HEAD where it was")
			_, statErr := os.Stat(filepath.Join(root, "retained.txt"))
			require.True(t, os.IsNotExist(statErr), "a refused merge must not materialize the retained bytes")
		})
	}
}
