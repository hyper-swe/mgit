package testutil

import (
	"os"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/require"
)

// NewTestGitRepo creates a temporary go-git repository with an initial commit.
// Returns the repository and a cleanup function.
// The cleanup function should be deferred by the caller.
// Refs: MGIT-1.2.5, NFR-4
func NewTestGitRepo(t *testing.T) (*git.Repository, func()) {
	t.Helper()

	tmpDir, err := os.MkdirTemp("", "mgit-test-repo-*")
	require.NoError(t, err, "failed to create temp dir for test repo")

	repo, err := git.PlainInit(tmpDir, false)
	require.NoError(t, err, "failed to init test git repo")

	// Create an initial commit so HEAD exists
	wt, err := repo.Worktree()
	require.NoError(t, err, "failed to get worktree")

	// Create a file to commit
	readmePath := tmpDir + "/README.md"
	err = os.WriteFile(readmePath, []byte("# test repo\n"), 0o600)
	require.NoError(t, err, "failed to write test file")

	_, err = wt.Add("README.md")
	require.NoError(t, err, "failed to add test file")

	_, err = wt.Commit("initial commit", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "test-agent",
			Email: "test@mgit.dev",
			When:  time.Date(2026, 4, 7, 12, 0, 0, 0, time.UTC),
		},
	})
	require.NoError(t, err, "failed to create initial commit")

	cleanup := func() {
		_ = os.RemoveAll(tmpDir)
	}

	return repo, cleanup
}
