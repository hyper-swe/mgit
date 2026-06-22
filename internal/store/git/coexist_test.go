package git

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestInit_OverExistingGitRepo_Coexists is the executable specification for
// mgit's first-level requirement: mgit runs INSIDE a git-managed project and
// operates OVER AND ABOVE the existing git repo — it must initialize in a
// directory that already has a real `.git`, and it must NEVER create,
// clobber, read, or mutate the project's `.git` (the project's history is
// sacrosanct host state).
//
// It is SKIPPED until the storage re-architecture lands (MGIT-14): today
// Init passes the project root as the go-git worktree, so go-git writes a
// `.git` gitfile at the root and Init fails with "open <root>/.git: is a
// directory" against a real repo. When MGIT-14.2 makes `.mgit/` a
// self-contained (worktree-less) store, remove the t.Skip and this must pass.
//
// This test is the guardrail whose absence let the gap ship: every existing
// test inits mgit in a fresh EMPTY temp dir, so only the greenfield path was
// ever exercised. Refs: MGIT-14, ADR-001 (amendment 2026-06-22)
func TestInit_OverExistingGitRepo_Coexists(t *testing.T) {
	t.Skip("MGIT-14: mgit must operate over an existing git repo (.mgit self-contained, project .git untouched). Un-skip when MGIT-14.2 lands.")

	dir := t.TempDir()
	clk := func() time.Time { return time.Date(2026, 6, 22, 12, 0, 0, 0, time.UTC) }

	// A real git project with pre-existing history.
	projGit, err := gogit.PlainInit(dir, false)
	require.NoError(t, err)
	wt, err := projGit.Worktree()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o600))
	_, err = wt.Add("main.go")
	require.NoError(t, err)
	sig := &object.Signature{Name: "dev", Email: "dev@x", When: clk()}
	historyHead, err := wt.Commit("real project history", &gogit.CommitOptions{Author: sig, Committer: sig})
	require.NoError(t, err)

	gitInfo, err := os.Stat(filepath.Join(dir, ".git"))
	require.NoError(t, err)
	require.True(t, gitInfo.IsDir(), "precondition: the project .git is a real directory")

	// mgit initializes OVER the existing git project — must succeed.
	repo, err := Init(dir, clk)
	require.NoError(t, err, "mgit must initialize in an existing git project")
	t.Cleanup(func() { _ = repo.Close() })

	// The project's git is untouched: still a real .git directory (not turned
	// into a gitfile), and its HEAD/history are byte-for-byte unchanged.
	after, err := os.Stat(filepath.Join(dir, ".git"))
	require.NoError(t, err)
	assert.True(t, after.IsDir(), "mgit must never replace the project's .git directory with a gitfile")

	reopened, err := gogit.PlainOpen(dir)
	require.NoError(t, err, "the project's own git repo must still open normally")
	head, err := reopened.Head()
	require.NoError(t, err)
	assert.Equal(t, historyHead, head.Hash(), "mgit must not move or rewrite the project's HEAD")

	// (MGIT-14.3/14.4 extend this with commit + worktree over the same repo;
	// MGIT-14.6 promotes it to a full-lifecycle dogfood e2e in CI.)
}
