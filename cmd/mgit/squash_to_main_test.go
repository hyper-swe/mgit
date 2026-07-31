package main

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSquashToMain_FromLinkedWorktree_Refused is the cmd-level proof of the
// worktree guard in squash.go's RunE: --to-main switches the shared HEAD to
// main, which from a linked worktree would mutate the shared parent's HEAD
// out from under it (and target the wrong branch). The guard's underlying
// promote-to-main behavior is proven end to end by
// e2e/squash_semantics_e2e_test.go; nothing at the cmd layer previously
// exercised the guard itself. Refs: MGIT-24, MGIT-61.13
func TestSquashToMain_FromLinkedWorktree_Refused(t *testing.T) {
	repo := hostRepoWithCommit(t, "MAIN-1")

	require.NoError(t, runCLI(t, "worktree", "add", "wt", "--task-id", "MAIN-2"), "worktree add")
	t.Chdir(filepath.Join(repo, "wt"))

	err := runCLI(t, "commit", "-m", "wt change", "--allow-empty")
	require.NoError(t, err, "commit from inside the worktree")
	err = runCLI(t, "squash", "--task-id", "MAIN-2", "--to-main")

	require.Error(t, err, "a linked worktree must not be able to promote to main")
	assert.Contains(t, err.Error(), "linked worktree")
	assert.Contains(t, err.Error(), "MAIN-2", "the error names the bound task")
}

// TestSquashToMain_FromTheParentRepo_Allowed is the control: the same
// --to-main call from the repo a worktree was added FROM (not the worktree
// itself, and with no worktree bound to THIS task) must not trip the guard
// at all. Deliberately does not reuse hostRepoWithCommit, which already
// squashes its task once -- squashing the same task again here would test
// "nothing new to squash" semantics rather than the guard.
func TestSquashToMain_FromTheParentRepo_Allowed(t *testing.T) {
	repo := t.TempDir()
	t.Chdir(repo)
	require.NoError(t, runCLI(t, "init"), "mgit init")
	require.NoError(t, runCLI(t, "commit", "-m", "root commit", "--task-id", "MAIN-3", "--allow-empty"))

	err := runCLI(t, "squash", "--task-id", "MAIN-3", "--to-main")

	require.NoError(t, err, "the parent repo is not a linked worktree; --to-main must proceed")
}
