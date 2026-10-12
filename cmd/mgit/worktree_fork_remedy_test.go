package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
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

// FORK, SALVAGE, SQUASH: THE PRESCRIBED LOOP LANDS (MGIT-82). The guidance
// forks a new task worktree at a good commit and salvages the still-good work
// from the old line with `mgit cherry-pick`. In a bound worktree the pick was
// recorded under the SOURCE commit's task, so the new task's log did not show
// it, its squash said "task not found", and after a commit of its own the
// squash failed verification (the pick sat above the pinned fork-base under
// another task). A pick in a task worktree belongs to that worktree's task, as
// a commit there does. Refs: MGIT-82, MGIT-24, FR-16
func TestWorktree_ForkSalvageSquash_RoundTrip(t *testing.T) {
	root := projectWithGit(t)
	wt := filepath.Join(t.TempDir(), "wt")
	require.NoError(t, runCLI(t, "worktree", "add", wt, "--task-id", "MGIT-82"))
	t.Chdir(wt)
	require.NoError(t, os.WriteFile(filepath.Join(wt, "step.txt"), []byte("good\n"), 0o600))
	require.NoError(t, runCLI(t, "commit", "-a", "-m", "the good step"))
	good := lastCommitShort(t)
	require.NoError(t, os.WriteFile(filepath.Join(wt, "step.txt"), []byte("wrong\n"), 0o600))
	require.NoError(t, runCLI(t, "commit", "-a", "-m", "the wrong step"))
	require.NoError(t, os.WriteFile(filepath.Join(wt, "useful.txt"), []byte("still good\n"), 0o600))
	require.NoError(t, runCLI(t, "commit", "-a", "-m", "a still-good step"))
	useful := lastCommitShort(t)

	t.Chdir(root)
	fork := filepath.Join(t.TempDir(), "fork")
	require.NoError(t, runCLI(t, "work", fork, "--task-id", "MGIT-82.1", "--base", good))
	t.Chdir(fork)
	require.NoError(t, runCLI(t, "cherry-pick", useful), "salvage the still-good step onto the new line")
	require.NoError(t, os.WriteFile(filepath.Join(fork, "step.txt"), []byte("the right way\n"), 0o600))
	require.NoError(t, runCLI(t, "commit", "-a", "-m", "the new approach"))

	log, _, err := runCLICap(t, "log", "--task-id", "MGIT-82.1")
	require.NoError(t, err)
	assert.Contains(t, log, "cherry-pick", "the salvaged step is on the new task")
	assert.Contains(t, log, "the new approach")

	patch, _, err := runCLICap(t, "squash", "--task-id", "MGIT-82.1", "--to-git", "--dry-run")
	require.NoError(t, err, "the new task squashes")
	assert.Contains(t, patch, "useful.txt", "the salvaged work lands")
	assert.Contains(t, patch, "the right way")
	assert.NotContains(t, patch, "wrong", "the wrong step does not")
}

// A contradicting --task-id on a pick in a task worktree is refused, as on a
// commit there: work is never recorded under another task. Refs: MGIT-82, MGIT-24
func TestWorktree_CherryPickWithAnotherTaskID_Refused(t *testing.T) {
	projectWithGit(t)
	wt := filepath.Join(t.TempDir(), "wt")
	require.NoError(t, runCLI(t, "worktree", "add", wt, "--task-id", "MGIT-82"))
	t.Chdir(wt)
	require.NoError(t, os.WriteFile(filepath.Join(wt, "a.txt"), []byte("a\n"), 0o600))
	require.NoError(t, runCLI(t, "commit", "-a", "-m", "a step"))
	h := lastCommitShort(t)
	require.NoError(t, runCLI(t, "rollback", "--commit", h))

	err := runCLI(t, "cherry-pick", h, "--task-id", "MGIT-999")

	require.ErrorIs(t, err, model.ErrTaskMismatch)
}
