// Package e2e: the mgit-over-an-existing-git-repo dogfood guardrail.
// This is the check whose absence let the "mgit can't run in a git repo"
// blocker ship (MGIT-14): every other test inits mgit in an empty temp dir,
// so only the greenfield path was ever exercised. This drives the REAL mgit
// binary through a full lifecycle inside a REAL git project with history and
// proves the project's .git is never touched. Refs: MGIT-14, MGIT-14.6, ADR-001
package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gitCmd runs a real git command in dir (the existing-project substrate mgit
// must coexist with).
func gitCmd(t *testing.T, dir string, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	full := append([]string{"-c", "user.email=dev@example.com", "-c", "user.name=dev"}, args...)
	cmd := exec.CommandContext(ctx, "git", full...) //nolint:gosec // fixed args, test
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, string(out))
}

// snapshotProjectGit returns every file under <dir>/.git keyed by its relative
// path with exact bytes; comparing two snapshots proves mgit never wrote into
// the project's git repository.
func snapshotProjectGit(t *testing.T, dir string) map[string]string {
	t.Helper()
	root := filepath.Join(dir, ".git")
	snap := make(map[string]string)
	require.NoError(t, filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		data, rerr := os.ReadFile(p) //nolint:gosec // test path under .git
		if rerr != nil {
			return rerr
		}
		snap[rel] = string(data)
		return nil
	}))
	return snap
}

// TestE2E_FullLifecycle_OverRealGitRepo_HistoryIntact drives the real mgit
// binary through init -> add -> commit -> worktree inside a real git project
// and asserts the project's .git is byte-for-byte unchanged afterward.
func TestE2E_FullLifecycle_OverRealGitRepo_HistoryIntact(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available")
	}
	bin := buildMgitBinary(t)
	repoDir := t.TempDir()

	// A real git project with pre-existing history — exactly a user's repo.
	gitCmd(t, repoDir, "init")
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, "main.go"), []byte("package main\n"), 0o600))
	gitCmd(t, repoDir, "add", "main.go")
	gitCmd(t, repoDir, "commit", "-m", "real project history")

	info, err := os.Stat(filepath.Join(repoDir, ".git"))
	require.NoError(t, err)
	require.True(t, info.IsDir(), "precondition: the project .git is a real directory")
	before := snapshotProjectGit(t, repoDir)

	// mgit runs OVER the existing git project — the whole point of MGIT-14.
	out, err := runMgit(t, bin, repoDir, "init")
	require.NoError(t, err, "mgit init over an existing git repo must succeed: %s", out)

	require.NoError(t, os.WriteFile(filepath.Join(repoDir, "feature.go"), []byte("package feature\n"), 0o600))
	out, err = runMgit(t, bin, repoDir, "add", "feature.go")
	require.NoError(t, err, "mgit add: %s", out)

	out, err = runMgit(t, bin, repoDir, "commit", "--task-id", "MGIT-14", "-m", "add feature")
	require.NoError(t, err, "mgit commit: %s", out)
	assert.Contains(t, out, "MGIT-14", "the commit is task-tagged")

	out, err = runMgit(t, bin, repoDir, "worktree", "add", "--task", "MGIT-14.6", "./wt")
	require.NoError(t, err, "mgit worktree add over a real git repo must succeed: %s", out)
	out, err = runMgit(t, bin, repoDir, "worktree", "list")
	require.NoError(t, err, "mgit worktree list: %s", out)
	assert.Contains(t, out, "MGIT-14.6", "the worktree is registered")

	// The project's git is byte-for-byte unchanged: still a real .git directory
	// (not turned into a gitfile) and identical contents.
	after, err := os.Stat(filepath.Join(repoDir, ".git"))
	require.NoError(t, err)
	assert.True(t, after.IsDir(), "mgit must not replace the project's .git directory")
	assert.Equal(t, before, snapshotProjectGit(t, repoDir),
		"the project's .git must be byte-for-byte unchanged after a full mgit CLI lifecycle")

	// The project's own git still operates normally alongside mgit.
	gitCmd(t, repoDir, "status")
}
