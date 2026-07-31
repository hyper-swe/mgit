// Package e2e — squash branch-shape guardrail (MGIT-22).
// Drives the REAL mgit binary through the exact dogfood scenario that exposed
// the bug: two MGIT-1 micro-commits then an unrelated MGIT-2 commit, squash
// MGIT-1. It pins the chosen semantics: the squash lands on task/MGIT-1 off the
// task base, main is NOT advanced, the originals are retained, and --to-main
// genuinely promotes the squash into main. Refs: FR-7, FR-7.2a, FR-12, MGIT-22
package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestE2E_Squash_LandsOnTaskBranch_MainUntouched is the CLI regression for the
// dogfood bug. Refs: FR-7.2a, MGIT-22
func TestE2E_Squash_LandsOnTaskBranch_MainUntouched(t *testing.T) {
	bin := buildMgitBinary(t)
	repoDir := t.TempDir()
	gitCmd(t, repoDir, "init")
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, "readme.md"), []byte("base\n"), 0o600))
	gitCmd(t, repoDir, "add", "readme.md")
	gitCmd(t, repoDir, "commit", "-m", "base")

	mustMgit(t, bin, repoDir, "init")
	commitFile(t, bin, repoDir, "MGIT-1", "a.go", "package a\n")
	commitFile(t, bin, repoDir, "MGIT-1", "b.go", "package b\n")
	commitFile(t, bin, repoDir, "MGIT-2", "c.go", "package c\n")

	logBefore := mustMgit(t, bin, repoDir, "log")

	out := mustMgit(t, bin, repoDir, "squash", "--task-id", "MGIT-1")
	assert.Contains(t, out, "task/MGIT-1", "squash output must name the task branch it landed on")

	// main is byte-for-byte the same log: no squash commit appended, originals kept.
	logAfter := mustMgit(t, bin, repoDir, "log")
	assert.Equal(t, logBefore, logAfter, "squash must not change the main branch log")

	// The dedicated task branch now exists.
	branches := mustMgit(t, bin, repoDir, "branch", "list")
	assert.Contains(t, branches, "task/MGIT-1", "squash must create the task/MGIT-1 branch")

	// --to-main genuinely advances main (a merge commit) while keeping originals.
	promote := mustMgit(t, bin, repoDir, "squash", "--task-id", "MGIT-1", "--to-main")
	assert.Contains(t, promote, "Promoted squash to main", "--to-main must report promotion")
	logPromoted := mustMgit(t, bin, repoDir, "log")
	assert.NotEqual(t, logAfter, logPromoted, "--to-main must advance main")
	assert.Contains(t, logPromoted, "MGIT-1", "original task-1 micro-commits remain after promote")
	assert.Contains(t, logPromoted, "MGIT-2", "the unrelated commit remains after promote")
}

// TestE2E_SquashToMain_FromLinkedWorktree_Refused is the real-binary e2e for
// the worktree guard: squash.go's RunE refuses --to-main from a linked
// worktree because promoting would switch the shared parent HEAD out from
// under it. TestE2E_Squash_LandsOnTaskBranch_MainUntouched above proves
// --to-main's promote behavior end to end; TestE2E_Worktree_
// GuardsAgainstSharedHeadMutation (worktree_commit_e2e_test.go) proves the
// command is among several a worktree must reject, but only generically
// (assert.Error, no message check). Neither pins the guard's own contract —
// that it names "linked worktree" and the bound task, and that main is left
// untouched by the refusal — the way cmd/mgit/squash_to_main_test.go already
// does at the cmd (in-process) layer. This is that same proof through the
// real compiled binary. Refs: MGIT-24, MGIT-61.13, FR-7.2a
func TestE2E_SquashToMain_FromLinkedWorktree_Refused(t *testing.T) {
	bin := buildMgitBinary(t)
	repoDir := t.TempDir()
	gitCmd(t, repoDir, "init")
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, "readme.md"), []byte("base\n"), 0o600))
	gitCmd(t, repoDir, "add", "readme.md")
	gitCmd(t, repoDir, "commit", "-m", "base")

	mustMgit(t, bin, repoDir, "init")
	commitFile(t, bin, repoDir, "MAIN-1", "seed.txt", "seed\n")
	mustMgit(t, bin, repoDir, "squash", "--task-id", "MAIN-1") // creates task/MAIN-1 to worktree from

	wt := filepath.Join(repoDir, "wt")
	mustMgit(t, bin, repoDir, "worktree", "add", "--task", "MAIN-2", wt)
	require.NoError(t, os.WriteFile(filepath.Join(wt, "wt.txt"), []byte("wt change\n"), 0o600))
	mustMgit(t, bin, wt, "add", "wt.txt")
	mustMgit(t, bin, wt, "commit", "-m", "wt change")

	mainLogBefore := mustMgit(t, bin, repoDir, "log")

	out, err := runMgit(t, bin, wt, "squash", "--task-id", "MAIN-2", "--to-main")
	require.Error(t, err, "a linked worktree must not be able to promote to main")
	assert.Contains(t, out, "linked worktree")
	assert.Contains(t, out, "MAIN-2", "the error names the bound task")

	// The refusal is a genuine no-op: the parent's main log is byte-for-byte
	// unchanged, not just "an error was returned but who knows what ran first".
	mainLogAfter := mustMgit(t, bin, repoDir, "log")
	assert.Equal(t, mainLogBefore, mainLogAfter, "a refused --to-main must not touch main")
}

// mustMgit runs mgit and fails the test on error, returning stdout.
func mustMgit(t *testing.T, bin, repoDir string, args ...string) string {
	t.Helper()
	out, err := runMgit(t, bin, repoDir, args...)
	require.NoError(t, err, "mgit %s: %s", strings.Join(args, " "), out)
	return out
}

// commitFile writes content to file under repoDir, stages it, and commits it
// tagged with task via the real mgit binary.
func commitFile(t *testing.T, bin, repoDir, task, file, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, file), []byte(content), 0o600))
	mustMgit(t, bin, repoDir, "add", file)
	mustMgit(t, bin, repoDir, "commit", "--task-id", task, "-m", "edit "+file)
}
