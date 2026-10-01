// Package e2e — `mgit work` materializes every file git tracks, whatever the
// ignore rules say (MGIT-277). On 0.6.8 a file tracked although an ignore
// rule matched it (added past the rule with `git add -f`) was missing from
// the new task's worktree, and a consumer comparing the worktree with the
// base read the absence as a deletion. The store-level fix and its unit tests
// are MGIT-269; this holds the reported shape on the binary. Ignore rules
// decide what is untracked, never what the base already tracks.
// Refs: MGIT-277, MGIT-269
package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeIgnoredFixture(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, rel)
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o750))
	require.NoError(t, os.WriteFile(p, []byte(content), 0o600))
}

// readIgnoredFixture returns a file's content, or "" when it is absent.
func readIgnoredFixture(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // test-owned path
	if os.IsNotExist(err) {
		return ""
	}
	require.NoError(t, err)
	return string(data)
}

func TestE2E_Work_TrackedFileMatchedByAnIgnoreRule_IsMaterializedAndNotDeleted(t *testing.T) {
	bin := buildMgitBinary(t)
	tests := []struct {
		name    string
		ignore  string // where the rule lives, relative to the project
		rule    string
		tracked string // force-added past the rule
	}{
		{name: "root_gitignore", ignore: ".gitignore", rule: "*.local\n.mgit/\n", tracked: "keep.local"},
		{name: "ignored_directory", ignore: ".gitignore", rule: "build/\n.mgit/\n", tracked: "build/tracked.txt"},
		{name: "nested_gitignore", ignore: "d/.gitignore", rule: "*.local\n", tracked: "d/keep.local"},
		{name: "info_exclude", ignore: ".git/info/exclude", rule: "*.local\n", tracked: "keep.local"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := t.TempDir()
			gitCmd(t, repo, "init")
			if tt.ignore != ".gitignore" {
				writeIgnoredFixture(t, repo, ".gitignore", ".mgit/\n")
			}
			writeIgnoredFixture(t, repo, tt.ignore, tt.rule)
			writeIgnoredFixture(t, repo, "a.txt", "a\n")
			writeIgnoredFixture(t, repo, tt.tracked, "tracked past the rule\n")
			gitCmd(t, repo, "add", "-A") // honors the rule: the tracked file is left out
			gitCmd(t, repo, "add", "-f", tt.tracked)
			gitCmd(t, repo, "commit", "-m", "seed")
			mustMgit(t, bin, repo, "init")

			wt := filepath.Join(t.TempDir(), "wt")
			mustMgit(t, bin, repo, "work", wt, "--task-id", "MGIT-277.E2E")

			assert.Equal(t, "tracked past the rule\n", readIgnoredFixture(t, filepath.Join(wt, tt.tracked)),
				"the worktree holds the tracked file with its content")
			status := mustMgit(t, bin, wt, "status")
			assert.NotContains(t, status, filepath.Base(tt.tracked), "status in the worktree reports nothing for it")

			// A consumer compares the task with its base: the task's own diff
			// and its squashed patch hold the task's work and nothing else.
			writeIgnoredFixture(t, wt, "work.txt", "task work\n")
			mustMgit(t, bin, wt, "commit", "-a", "-m", "task work")
			diff := mustMgit(t, bin, wt, "diff", "--task-id", "MGIT-277.E2E", "--stat")
			assert.Contains(t, diff, "work.txt")
			assert.NotContains(t, diff, filepath.Base(tt.tracked), "the task's diff does not touch it")
			patch, err := runMgit(t, bin, wt, "squash", "--task-id", "MGIT-277.E2E", "--to-git")
			require.NoError(t, err, patch)
			assert.Contains(t, patch, "work.txt")
			assert.False(t, strings.Contains(patch, filepath.Base(tt.tracked)),
				"the squashed patch does not delete the tracked file:\n%s", patch)
		})
	}
}
