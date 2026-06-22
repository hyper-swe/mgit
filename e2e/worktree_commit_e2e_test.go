// Package e2e — committing from within a linked worktree (MGIT-24 / ADR-007).
// Drives the real mgit binary: `worktree add`, then `add`+`commit` run FROM the
// worktree land on the bound task branch, share the parent store, auto-inherit
// the bound task, and leave the parent's main untouched. Refs: FR-16, MGIT-24
package e2e

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestE2E_CommitFromWorktree_LandsOnBoundBranch(t *testing.T) {
	bin := buildMgitBinary(t)
	repoDir := t.TempDir()
	gitCmd(t, repoDir, "init")
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, "main.go"), []byte("package main\n"), 0o600))
	mustMgit(t, bin, repoDir, "init")
	mustMgit(t, bin, repoDir, "add", "main.go")
	mustMgit(t, bin, repoDir, "commit", "--task-id", "MGIT-1", "-m", "seed")

	// Two worktrees on disjoint task branches.
	wtA := filepath.Join(repoDir, "wt-a")
	wtB := filepath.Join(repoDir, "wt-b")
	mustMgit(t, bin, repoDir, "worktree", "add", "--task", "MGIT-2", wtA)
	mustMgit(t, bin, repoDir, "worktree", "add", "--task", "MGIT-3", wtB)

	// Commit from inside worktree A with NO --task-id: must auto-inherit MGIT-2.
	require.NoError(t, os.WriteFile(filepath.Join(wtA, "a.go"), []byte("package a\n"), 0o600))
	mustMgit(t, bin, wtA, "add", "a.go")
	outA := mustMgit(t, bin, wtA, "commit", "-m", "work A")
	assert.Contains(t, outA, "MGIT-2", "worktree A commit auto-inherits its bound task")

	// Commit from worktree B, also auto-inherited (disjoint branch).
	require.NoError(t, os.WriteFile(filepath.Join(wtB, "b.go"), []byte("package b\n"), 0o600))
	mustMgit(t, bin, wtB, "add", "b.go")
	mustMgit(t, bin, wtB, "commit", "-m", "work B")

	// Each worktree's log shows its OWN commit, not the other's.
	logA := mustMgit(t, bin, wtA, "log")
	assert.Contains(t, logA, "work A")
	assert.NotContains(t, logA, "work B", "worktree A must not see worktree B's branch")
	logB := mustMgit(t, bin, wtB, "log")
	assert.Contains(t, logB, "work B")
	assert.NotContains(t, logB, "work A")

	// The parent's main is untouched by either worktree commit.
	mainLog := mustMgit(t, bin, repoDir, "log")
	assert.NotContains(t, mainLog, "work A", "parent main must not contain a worktree commit")
	assert.NotContains(t, mainLog, "work B")

	// Both task branches advanced in the shared store, visible from the parent.
	branches := mustMgit(t, bin, repoDir, "branch", "list")
	assert.Contains(t, branches, "task/MGIT-2")
	assert.Contains(t, branches, "task/MGIT-3")

	// The project's git repo is never touched by mgit (MGIT-14 invariant).
	info, err := os.Stat(filepath.Join(repoDir, ".git"))
	require.NoError(t, err)
	assert.True(t, info.IsDir(), "project .git must remain a real directory")
}

// TestE2E_Worktree_GuardsAgainstSharedHeadMutation verifies a linked worktree
// rejects operations that would mutate the shared parent HEAD or touch a task
// other than the one it is bound to. Refs: MGIT-24
func TestE2E_Worktree_GuardsAgainstSharedHeadMutation(t *testing.T) {
	bin := buildMgitBinary(t)
	repoDir := t.TempDir()
	gitCmd(t, repoDir, "init")
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, "main.go"), []byte("package main\n"), 0o600))
	mustMgit(t, bin, repoDir, "init")
	mustMgit(t, bin, repoDir, "add", "main.go")
	mustMgit(t, bin, repoDir, "commit", "--task-id", "MGIT-1", "-m", "seed")

	wt := filepath.Join(repoDir, "wt")
	mustMgit(t, bin, repoDir, "worktree", "add", "--task", "MGIT-2", wt)

	// Each of these would mutate the shared HEAD or a foreign task; all must fail.
	for _, args := range [][]string{
		{"checkout", "main"},
		{"branch", "main"},
		{"squash", "--task-id", "MGIT-2", "--to-main"},
		{"cherry-pick", "deadbeef", "--onto", "main"},
		{"rollback", "--task-id", "MGIT-1"}, // foreign task
	} {
		_, err := runMgit(t, bin, wt, args...)
		assert.Error(t, err, "worktree must reject: mgit %v", args)
	}

	// The parent's HEAD/main is intact and still resolves after the rejections.
	out := mustMgit(t, bin, repoDir, "status")
	assert.Contains(t, out, "main", "parent HEAD must still be on main after worktree rejections")
}
