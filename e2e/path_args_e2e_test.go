// Package e2e — a path given to commit, status or diff (MGIT-282). None of
// the three scopes to a path; each silently dropped one, so `mgit commit -m x
// pkg` recorded everything staged. A path is now refused, before anything is
// recorded or printed, naming what was given and the way to do it.
// Refs: MGIT-282
package e2e

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pathArgsRepo has one task commit and two staged files, pkg/a.go and b.go.
func pathArgsRepo(t *testing.T) (bin, repo string) {
	t.Helper()
	bin = buildMgitBinary(t)
	repo = t.TempDir()
	mustMgit(t, bin, repo, "init")
	commitFile(t, bin, repo, "MGIT-282", "seed.txt", "seed\n")
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "pkg"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "pkg", "a.go"), []byte("package pkg\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "b.go"), []byte("package b\n"), 0o600))
	mustMgit(t, bin, repo, "add", "pkg/a.go")
	mustMgit(t, bin, repo, "add", "b.go")
	return bin, repo
}

func TestE2E_CommitWithAPath_IsRefusedAndRecordsNothing(t *testing.T) {
	bin, repo := pathArgsRepo(t)
	before := mustMgit(t, bin, repo, "log")

	out, err := runMgit(t, bin, repo, "commit", "--task-id", "MGIT-282", "-m", "only pkg", "pkg")
	require.Error(t, err, "a path to commit must be refused, not dropped: %s", out)
	assert.Contains(t, out, "pkg", "the refusal names what was given")
	assert.Contains(t, out, "mgit restore --staged", "the refusal gives the way to commit less")
	assert.Equal(t, before, mustMgit(t, bin, repo, "log"), "nothing is recorded")
}

func TestE2E_StatusWithAPath_IsRefused(t *testing.T) {
	bin, repo := pathArgsRepo(t)
	out, err := runMgit(t, bin, repo, "status", "pkg")
	require.Error(t, err, "status of a path must be refused, not answered for the whole tree: %s", out)
	assert.Contains(t, out, "pkg")
	assert.NotContains(t, out, "b.go", "nothing about the whole tree is printed")
}

func TestE2E_DiffWithAPath_IsRefused(t *testing.T) {
	bin, repo := pathArgsRepo(t)
	out, err := runMgit(t, bin, repo, "diff", "--task-id", "MGIT-282", "pkg")
	require.Error(t, err, "a path to diff must be refused, not dropped: %s", out)
	assert.Contains(t, out, "pkg")
}

// Without a path, each still works.
func TestE2E_CommitStatusDiffWithoutAPath_StillWork(t *testing.T) {
	bin, repo := pathArgsRepo(t)
	mustMgit(t, bin, repo, "status")
	mustMgit(t, bin, repo, "commit", "--task-id", "MGIT-282", "-m", "both")
	mustMgit(t, bin, repo, "diff", "--task-id", "MGIT-282")
}

// MGIT-284: squash took no positional argument either, so `squash --to-git
// <path>` exported the whole task. It is refused and exports nothing.
func TestE2E_SquashToGitWithAPath_IsRefusedAndExportsNothing(t *testing.T) {
	bin, repo := pathArgsRepo(t)
	mustMgit(t, bin, repo, "commit", "--task-id", "MGIT-282", "-m", "both")

	out, err := runMgit(t, bin, repo, "squash", "--task-id", "MGIT-282", "--to-git", "pkg")
	require.Error(t, err, "a path to squash must be refused, not dropped: %s", out)
	assert.Contains(t, out, "pkg")
	assert.NotContains(t, out, "diff --git", "no patch is exported")
}
