package e2e

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The recreate path a consumer runs after the OS clears a worktree's
// directory, with the project's main branch moving in the meantime: a file
// ADDED and committed in the task worktree must come back in the rebuilt
// worktree and in the task's diff and patch, and the commit that moved main
// must not appear as the task's work. A consumer once landed only the file it
// edited after the rebuild and lost the added one; the mgit side of that
// sequence is pinned here, so a change in what a re-add materializes or pins
// is seen at once. Refs: MGIT-275, MGIT-35, ADR-008 §4
func TestE2E_WorktreeReAdded_AfterMainMoves_KeepsTheAddedFile(t *testing.T) {
	f := newReaddFixture(t) // one.txt added and committed; the directory lost and pruned

	require.NoError(t, os.WriteFile(filepath.Join(f.repo, "moved.txt"), []byte("main moved\n"), 0o600))
	gitCmd(t, f.repo, "add", "moved.txt")
	gitCmd(t, f.repo, "commit", "-m", "main moves")

	mustMgit(t, f.bin, f.repo, "work", f.wt, "--task-id", f.task)

	kept, err := os.ReadFile(filepath.Join(f.wt, "one.txt")) //nolint:gosec // G304: a path built from t.TempDir in this test
	require.NoError(t, err, "the file added before the loss must be in the rebuilt worktree")
	assert.Equal(t, "one\n", string(kept))
	assert.Equal(t, f.forkBase, pinnedForkBase(t, f), "the re-add must pin the original fork-base, not main's new tip")

	f.commitSecond(t)
	diff := mustMgit(t, f.bin, f.repo, "diff", "--task-id", f.task)
	assert.Contains(t, diff, "one.txt", "the added file is part of the task's diff")
	assert.Contains(t, diff, "two.txt")
	assert.NotContains(t, diff, "moved.txt", "the commit that moved main is not the task's work")
	patch := mustMgit(t, f.bin, f.wt, "squash", "--task-id", f.task, "--to-git")
	assert.Contains(t, patch, "+one\n", "the added file is in the exported patch")
	assert.Contains(t, patch, "+two\n")
	assert.NotContains(t, patch, "main moved")
}
