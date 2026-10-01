// Package e2e — `mgit add` from a subdirectory, held to git (MGIT-278.1).
// Each case runs the same command from the same subdirectory of the same tree
// through git and through mgit, and the staged sets must be equal: `add .`
// stages only the subtree, and every path resolves against the working
// directory, as git does. Refs: MGIT-278.1, MGIT-278
package e2e

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// parityTree is a git project with mgit initialized over it and synced, then
// changed: modified, new, deleted and ignored files in pkg/ and outside it.
func parityTree(t *testing.T, bin string) string {
	t.Helper()
	repo := t.TempDir()
	gitCmd(t, repo, "init")
	files := map[string]string{
		".gitignore": "*.log\n", "pkg/a.go": "a\n", "pkg/del.go": "d\n",
		"pkg/sub/b.go": "b\n", "other/o.go": "o\n",
	}
	for rel, content := range files {
		writeParityFile(t, repo, rel, content)
	}
	gitCmd(t, repo, "add", "-A")
	gitCmd(t, repo, "commit", "-m", "seed")
	mustMgit(t, bin, repo, "init")
	mustMgit(t, bin, repo, "status") // the base absorbs git's committed tree
	writeParityFile(t, repo, "pkg/a.go", "a2\n")
	writeParityFile(t, repo, "pkg/sub/new.go", "n\n")
	writeParityFile(t, repo, "pkg/sub/x.log", "ignored\n")
	writeParityFile(t, repo, "other/o.go", "o2\n")
	require.NoError(t, os.Remove(filepath.Join(repo, "pkg", "del.go")))
	return repo
}

func writeParityFile(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, rel)
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o750))
	require.NoError(t, os.WriteFile(p, []byte(content), 0o600))
}

// gitStaged runs `git add <args>` in dir and returns the staged paths, then
// unstages them so the tree is as it was.
func gitStaged(t *testing.T, repo, dir string, args ...string) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	run := func(a ...string) string {
		cmd := exec.CommandContext(ctx, "git", a...) //nolint:gosec // fixed git verbs, test
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", a, out)
		return string(out)
	}
	run(append([]string{"add"}, args...)...)
	out := run("diff", "--cached", "--name-only", "--no-renames")
	run("-C", repo, "reset", "-q")
	return sortedLines(out)
}

// mgitStaged runs `mgit add <args>` in dir and returns the staged paths, then
// clears the staging area.
func mgitStaged(t *testing.T, bin, repo, dir string, args ...string) []string {
	t.Helper()
	mustMgit(t, bin, dir, append([]string{"add"}, args...)...)
	data, err := os.ReadFile(filepath.Join(repo, ".mgit", "staging.json")) //nolint:gosec // test-owned path
	require.NoError(t, err)
	var s struct {
		Paths []string `json:"paths"`
	}
	require.NoError(t, json.Unmarshal(data, &s))
	require.NoError(t, os.Remove(filepath.Join(repo, ".mgit", "staging.json")))
	sort.Strings(s.Paths)
	return s.Paths
}

func sortedLines(s string) []string {
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(s), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	sort.Strings(out)
	return out
}

func TestE2E_AddFromASubdirectory_StagesWhatGitStages(t *testing.T) {
	bin := buildMgitBinary(t)
	repo := parityTree(t, bin)
	sub := filepath.Join(repo, "pkg")
	for _, args := range [][]string{
		{"."},             // the subtree only
		{"a.go"},          // a path relative to the working directory
		{"sub"},           // a relative directory
		{"../other/o.go"}, // a relative path leaving the subdirectory
		{"del.go"},        // a relative path to a deleted file
		{"-A"},            // the whole tree, from anywhere
	} {
		want := gitStaged(t, repo, sub, args...)
		got := mgitStaged(t, bin, repo, sub, args...)
		assert.Equal(t, want, got, "`add %s` from pkg/: git staged %v, mgit %v", strings.Join(args, " "), want, got)
	}
}

func TestE2E_AddFromASubdirectory_APathOutsideTheProject_IsRefused(t *testing.T) {
	bin := buildMgitBinary(t)
	repo := parityTree(t, bin)
	out, err := runMgit(t, bin, filepath.Join(repo, "pkg"), "add", "../../outside.go")
	require.Error(t, err, out)
	_, statErr := os.Stat(filepath.Join(repo, ".mgit", "staging.json"))
	assert.True(t, os.IsNotExist(statErr), "nothing is staged")
}

func TestE2E_RestoreStagedFromASubdirectory_ResolvesRelativePaths(t *testing.T) {
	bin := buildMgitBinary(t)
	repo := parityTree(t, bin)
	mustMgit(t, bin, repo, "add", "-A")

	mustMgit(t, bin, filepath.Join(repo, "pkg"), "restore", "--staged", "sub")
	data, err := os.ReadFile(filepath.Join(repo, ".mgit", "staging.json")) //nolint:gosec // test-owned path
	require.NoError(t, err)
	assert.NotContains(t, string(data), "pkg/sub/", "`restore --staged sub` from pkg/ unstages pkg/sub")
	assert.Contains(t, string(data), "other/o.go", "and nothing else")
}
