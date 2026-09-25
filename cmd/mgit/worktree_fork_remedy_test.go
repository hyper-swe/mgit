package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A TASK WORKTREE'S REFUSAL TO SWITCH BRANCHES NAMES THE FORK THAT WORKS
// (MGIT-82). A linked worktree is bound to one branch (MGIT-24), so `mgit
// checkout -b` and `mgit branch <name>` are refused inside it, and the
// generated guidance told the agent to fork exactly that way. The refusal
// now says how to fork a new line from a good commit instead: a new task
// worktree at that commit, made from the project root. Refs: MGIT-82, MGIT-24
func TestWorktree_BranchSwitchRefusal_NamesTheForkCommand(t *testing.T) {
	root := projectWithGit(t)
	wt := filepath.Join(t.TempDir(), "wt")
	require.NoError(t, runCLI(t, "worktree", "add", wt, "--task-id", "MGIT-82"))
	t.Chdir(wt)

	for name, args := range map[string][]string{
		"checkout -b":   {"checkout", "-b", "scratch"},
		"checkout":      {"checkout", "main"},
		"branch switch": {"branch", "main"},
	} {
		err := runCLI(t, args...)
		require.Error(t, err, "%s is refused in a task worktree", name)
		msg := err.Error()
		assert.Contains(t, msg, "bound to task MGIT-82", "%s: the reason is kept", name)
		assert.Contains(t, msg, "mgit work <new-path> --task-id <new-task-id> --base <good-commit>", "%s: the refusal names the fork", name)
		assert.Contains(t, msg, root, "%s: the refusal names the project root to run it from", name)
	}
}

// The fork the refusal and the guidance name really forks: from the project
// root, a new task worktree at a good commit of the old line holds that
// commit's content, not the old line's later, wrong step. Refs: MGIT-82
func TestWorktree_ForkFromAGoodCommit_HoldsItsContent(t *testing.T) {
	root := projectWithGit(t)
	wt := filepath.Join(t.TempDir(), "wt")
	require.NoError(t, runCLI(t, "worktree", "add", wt, "--task-id", "MGIT-82"))
	t.Chdir(wt)
	require.NoError(t, os.WriteFile(filepath.Join(wt, "step.txt"), []byte("good\n"), 0o600))
	require.NoError(t, runCLI(t, "commit", "-a", "-m", "the good step"))
	good := lastCommitShort(t)
	require.NoError(t, os.WriteFile(filepath.Join(wt, "step.txt"), []byte("wrong\n"), 0o600))
	require.NoError(t, runCLI(t, "commit", "-a", "-m", "the wrong step"))

	t.Chdir(root)
	fork := filepath.Join(t.TempDir(), "fork")
	require.NoError(t, runCLI(t, "work", fork, "--task-id", "MGIT-82.1", "--base", good))

	got, err := os.ReadFile(filepath.Join(fork, "step.txt")) //nolint:gosec // test-controlled path
	require.NoError(t, err)
	assert.Equal(t, "good\n", string(got), "the fork holds the good commit's content")
}

// lastCommitShort returns the newest commit's short hash from `mgit log`.
func lastCommitShort(t *testing.T) string {
	t.Helper()
	out, _, err := runCLICap(t, "log", "--oneline", "-n", "1")
	require.NoError(t, err)
	fields := strings.Fields(out)
	require.NotEmpty(t, fields, "log output: %q", out)
	return fields[0]
}
