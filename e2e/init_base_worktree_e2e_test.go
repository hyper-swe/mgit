package e2e

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The base init records is what a linked worktree forks from: a task worked
// in one exports a patch of its own change only. Refs: FEAT-6.116
func TestE2E_WorktreeTaskAfterInitCarriesOnlyItsChange(t *testing.T) {
	bin := buildMgitBinary(t)
	repoDir := t.TempDir()
	gitCmd(t, repoDir, "init")
	existing := []string{"README.md", "main.go", "pkg/util.go", "scripts/run.sh", ".editorconfig", ".gitignore"}
	for _, rel := range existing {
		p := filepath.Join(repoDir, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o750))
		require.NoError(t, os.WriteFile(p, []byte("existing "+rel+"\n"), 0o600))
	}
	mustMgit(t, bin, repoDir, "init")

	wt := filepath.Join(repoDir, "wt")
	mustMgit(t, bin, repoDir, "worktree", "add", "--task", "MGIT-2", wt)
	require.NoError(t, os.WriteFile(filepath.Join(wt, "feature.go"), []byte("package main\n// feature\n"), 0o600))
	mustMgit(t, bin, wt, "commit", "-a", "-m", "add feature")
	patchPath := filepath.Join(t.TempDir(), "task.patch")
	mustMgit(t, bin, repoDir, "squash", "--task-id", "MGIT-2", "-m", "add feature", "--to-git", "--to-git-output", patchPath)

	raw, err := os.ReadFile(patchPath) //nolint:gosec // test fixture path
	require.NoError(t, err)
	assert.Contains(t, string(raw), "feature.go")
	for _, rel := range existing {
		assert.NotContains(t, string(raw), rel, "the worktree task's patch must not carry a file the project already had")
	}
}
