package main

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gitIn runs git in dir, failing the test on error. The guard itself never
// shells out; the TEST does, because a linked worktree is something git makes
// and the guard has to read what git made. Refs: MGIT-182
func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...) //nolint:gosec // fixed argv, test-only
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
}

// The pre-push hook runs the guard with --repo set to `git rev-parse
// --show-toplevel`, which inside a LINKED worktree is the worktree itself —
// a directory whose .git is a file naming the main repository's
// .git/worktrees/<name>. The refs live in the common dir. A guard that opens
// only the worktree's own gitdir sees no refs/heads at all and refuses the
// branch being pushed as nonexistent, which blocked every push from every
// linked worktree, including the ones `mgit work` provisions. Refs: MGIT-182
func TestRun_LinkedWorktree_ResolvesTheBranchThroughTheCommonDir(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH; only git can create the linked worktree this test needs")
	}
	dir := incidentClone(t)
	scratch := t.TempDir()

	clean := filepath.Join(scratch, "clean")
	gitIn(t, dir, "worktree", "add", "-b", "fix/from-main", clean, "main")
	var out, errb bytes.Buffer
	code := run([]string{"--repo", clean, "--branch", "fix/from-main"}, &out, &errb)
	assert.Equal(t, 0, code, "a clean branch checked out in a linked worktree must pass, got: %s", errb.String())
	assert.NotContains(t, errb.String(), "reference not found")

	// The rule itself must still fire from a linked worktree: the incident
	// branch (cut from an unmerged task branch) is refused there too.
	inherited := filepath.Join(scratch, "inherited")
	gitIn(t, dir, "worktree", "add", "--force", inherited, "fix/ci-retry")
	errb.Reset()
	code = run([]string{"--repo", inherited, "--branch", "fix/ci-retry"}, &out, &errb)
	assert.Equal(t, exitRefused, code, "the guard must keep refusing from a linked worktree: %s", errb.String())
}
