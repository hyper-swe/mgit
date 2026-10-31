package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The sandbox host root (where the registered guest base lives) must be the
// PARENT repository's, from the repository itself, from a subdirectory of it,
// from a linked worktree, and from a subdirectory of a worktree. A worktree's own
// .mgit holds only a marker, so deriving the root from the working directory
// reported "this repo has no guest base" for a repo that had one, and its remedy
// composed a second base. Refs: MGIT-100, MGIT-57
func TestSandboxHostRoot_IsTheParentReposFromEveryStandingPlace(t *testing.T) {
	repo := t.TempDir()
	t.Chdir(repo)
	require.NoError(t, runCLI(t, "init", "--no-agent-files"))
	wt := filepath.Join(t.TempDir(), "wt")
	require.NoError(t, runCLI(t, "work", "--task-id", "WT-100", wt))
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "sub", "deeper"), 0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(wt, "sub", "deeper"), 0o700))

	want := filepath.Join(canonicalPath(repo), ".mgit", "sandbox")
	for name, dir := range map[string]string{
		"repo":                  repo,
		"repo subdirectory":     filepath.Join(repo, "sub", "deeper"),
		"worktree":              wt,
		"worktree subdirectory": filepath.Join(wt, "sub", "deeper"),
	} {
		t.Run(name, func(t *testing.T) {
			t.Chdir(dir)
			got, err := sandboxHostRoot()
			require.NoError(t, err)
			assert.Equal(t, want, canonicalPath(filepath.Dir(got))+string(filepath.Separator)+filepath.Base(got),
				"the host root must be the parent repository's, not the worktree's marker directory")
		})
	}
}
