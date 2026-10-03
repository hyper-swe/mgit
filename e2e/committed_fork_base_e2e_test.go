// Package e2e — a new task's base is git's committed tree (MGIT-283, the
// founder's ruling R-H327 §2). Creating a task worktree used to absorb the
// main checkout's UNCOMMITTED files into the task's base, so a consumer that
// landed the task's tree landed private uncommitted content, and `mgit status`
// in the main checkout then read clean. Capturing uncommitted work is now
// opt-in (`--include-uncommitted`), and either way every uncommitted file is
// named. Refs: MGIT-283, ADR-008 §2
package e2e

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// uncommittedProject is a git project, mgit initialized over it, with an
// uncommitted edit to a tracked file and an untracked file in the checkout.
func uncommittedProject(t *testing.T) (bin, repo string) {
	t.Helper()
	bin = buildMgitBinary(t)
	repo = t.TempDir()
	gitCmd(t, repo, "init")
	writeProjectFile(t, repo, ".gitignore", ".mgit/\n")
	writeProjectFile(t, repo, "tracked.txt", "committed\n")
	gitCmd(t, repo, "add", "-A")
	gitCmd(t, repo, "commit", "-m", "seed")
	mustMgit(t, bin, repo, "init")
	writeProjectFile(t, repo, "tracked.txt", "PRIVATE uncommitted edit\n")
	writeProjectFile(t, repo, "untracked.txt", "PRIVATE untracked\n")
	return bin, repo
}

func writeProjectFile(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, rel)
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o750))
	require.NoError(t, os.WriteFile(p, []byte(content), 0o600))
}

func readFileOrEmpty(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // test-owned path
	if os.IsNotExist(err) {
		return ""
	}
	require.NoError(t, err)
	return string(data)
}

// THE REPORTED SHAPE. The task's base is git's committed tree: the task
// worktree holds the committed version of the tracked file and no untracked
// file, a commit in it lands only its own change, the main checkout still
// reports both files as changed, and `mgit work` names both as left out.
func TestE2E_WorkByDefault_BasesTheTaskOnGitsCommittedTree(t *testing.T) {
	bin, repo := uncommittedProject(t)
	wt := filepath.Join(t.TempDir(), "wt")

	out := mustMgit(t, bin, repo, "work", wt, "--task-id", "MGIT-283.E2E")

	assert.Equal(t, "committed\n", readFileOrEmpty(t, filepath.Join(wt, "tracked.txt")),
		"the task holds git's committed version, not the uncommitted edit")
	assert.Empty(t, readFileOrEmpty(t, filepath.Join(wt, "untracked.txt")), "an untracked file is not in the task")
	assert.Contains(t, out, "tracked.txt", "mgit work names the uncommitted file it left out")
	assert.Contains(t, out, "untracked.txt")

	writeProjectFile(t, wt, "work.txt", "task work\n")
	mustMgit(t, bin, wt, "add", "work.txt")
	mustMgit(t, bin, wt, "commit", "-m", "task work")
	patch := mustMgit(t, bin, wt, "squash", "--task-id", "MGIT-283.E2E", "--to-git")
	assert.Contains(t, patch, "work.txt")
	assert.NotContains(t, patch, "PRIVATE", "no uncommitted content reaches the task's patch")

	status := mustMgit(t, bin, repo, "status")
	assert.Contains(t, status, "tracked.txt", "the main checkout still reports its uncommitted edit")
	assert.Contains(t, status, "untracked.txt")
}

// Opting in captures the uncommitted work, and says so, file by file.
func TestE2E_WorkWithIncludeUncommitted_CapturesAndNamesIt(t *testing.T) {
	bin, repo := uncommittedProject(t)
	wt := filepath.Join(t.TempDir(), "wt")

	out := mustMgit(t, bin, repo, "work", wt, "--task-id", "MGIT-283.E2E", "--include-uncommitted")

	assert.Equal(t, "PRIVATE uncommitted edit\n", readFileOrEmpty(t, filepath.Join(wt, "tracked.txt")))
	assert.Equal(t, "PRIVATE untracked\n", readFileOrEmpty(t, filepath.Join(wt, "untracked.txt")))
	assert.Contains(t, out, "tracked.txt", "mgit work names every file it captured")
	assert.Contains(t, out, "untracked.txt")
}

// A task's fork base is mgit's own housekeeping commit, not task work, so it
// carries no index entry, and `mgit verify` must not fail over it. Review
// finding on #248: the posture job's core_loop.sh failed "verify passes".
// Here git moves after `mgit init`, so the new task's base differs from the
// mgit base and a fork-base commit is written. Refs: MGIT-283, FR-12
func TestE2E_WorkAfterGitMoved_VerifyStillPasses(t *testing.T) {
	bin, repo := uncommittedProject(t)
	writeProjectFile(t, repo, "later.txt", "committed after mgit init\n")
	gitCmd(t, repo, "add", "later.txt")
	gitCmd(t, repo, "commit", "-m", "later")
	wt := filepath.Join(t.TempDir(), "wt")

	mustMgit(t, bin, repo, "work", wt, "--task-id", "MGIT-283.V")
	assert.Equal(t, "committed after mgit init\n", readFileOrEmpty(t, filepath.Join(wt, "later.txt")))

	for _, dir := range []string{wt, repo} { // the worktree's walk starts at the fork base's branch
		out, err := runMgit(t, bin, dir, "verify")
		require.NoError(t, err, "verify in %s must pass with a task fork base present: %s", dir, out)
	}
}
