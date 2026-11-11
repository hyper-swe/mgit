package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A project whose git history committed another store (a `.mgit` folder below
// the root) still starts a task from its committed tree. The store's files are
// left out, each skipped folder is named once on stderr, and what was committed
// beside the store is in the task.
// Refs: MGIT-283

// nestedStoreProject is a git project with a store committed one and three
// folders down, and a committed file beside each, with mgit initialized over it.
func nestedStoreProject(t *testing.T) (bin, repo string) {
	t.Helper()
	bin = buildMgitBinary(t)
	repo = t.TempDir()
	gitCmd(t, repo, "init")
	writeProjectFile(t, repo, ".gitignore", ".mgit/\n")
	writeProjectFile(t, repo, "main.go", "package main\n")
	writeProjectFile(t, repo, "archive/keep.go", "package archive\n")
	writeProjectFile(t, repo, "archive/.mgit/payload-one", "ARCHIVED-ONE\n")
	writeProjectFile(t, repo, "a/b/c/keep.go", "package c\n")
	writeProjectFile(t, repo, "a/b/c/.mgit/payload-two", "ARCHIVED-TWO\n")
	writeProjectFile(t, repo, "a/b/c/.mgit/more/payload-three", "ARCHIVED-THREE\n")
	gitCmd(t, repo, "add", "-A")
	gitCmd(t, repo, "add", "-f", "archive/.mgit", "a/b/c/.mgit")
	gitCmd(t, repo, "commit", "-m", "seed")
	mustMgit(t, bin, repo, "init")
	return bin, repo
}

func assertNestedStoreLeftOut(t *testing.T, out, wt string) {
	t.Helper()
	assert.Equal(t, 1, strings.Count(out, "Skipped nested snapshot data in archive/.mgit\n"), out)
	assert.Equal(t, 1, strings.Count(out, "Skipped nested snapshot data in a/b/c/.mgit\n"), out)
	assert.Equal(t, 2, strings.Count(out, "Skipped nested snapshot data"), "one line per folder: %s", out)
	assert.NotContains(t, out, "ARCHIVED", "no archived content is printed")
	assert.NotContains(t, out, "payload", "no path inside a skipped folder is printed")
	assert.NotContains(t, out, "keep.go", "a committed sibling is not reported as missing")
	assert.Equal(t, "package archive\n", readFileOrEmpty(t, filepath.Join(wt, "archive", "keep.go")))
	assert.Equal(t, "package c\n", readFileOrEmpty(t, filepath.Join(wt, "a", "b", "c", "keep.go")))
	for _, gone := range []string{"archive/.mgit/payload-one", "a/b/c/.mgit/payload-two", "a/b/c/.mgit/more/payload-three"} {
		_, err := os.Stat(filepath.Join(wt, filepath.FromSlash(gone)))
		assert.True(t, os.IsNotExist(err), "%s is not in the task's worktree", gone)
	}
}

func TestE2E_WorkWithCommittedNestedStores_SkipsThemAndNamesEach(t *testing.T) {
	bin, repo := nestedStoreProject(t)
	wt := filepath.Join(t.TempDir(), "wt")

	out := mustMgit(t, bin, repo, "work", wt, "--task-id", "MGIT-283.NS")

	assertNestedStoreLeftOut(t, out, wt)
	assert.Equal(t, "ARCHIVED-ONE\n", readFileOrEmpty(t, filepath.Join(repo, "archive", ".mgit", "payload-one")),
		"the project's own copy of the store is untouched")
}

func TestE2E_WorktreeAddWithCommittedNestedStores_SkipsThemAndNamesEach(t *testing.T) {
	bin, repo := nestedStoreProject(t)
	wt := filepath.Join(t.TempDir(), "wt")

	out := mustMgit(t, bin, repo, "worktree", "add", wt, "--task-id", "MGIT-283.NS2")

	assertNestedStoreLeftOut(t, out, wt)
}

// --include-uncommitted takes the checkout's live state, whose walk is
// unchanged: it already passes over the whole folder that holds a store. The
// task is created and none of the store's files are in it.
func TestE2E_WorkIncludeUncommittedWithCommittedNestedStores_LiveWalkUnchanged(t *testing.T) {
	bin, repo := nestedStoreProject(t)
	wt := filepath.Join(t.TempDir(), "wt")

	_, err := runMgit(t, bin, repo, "work", wt, "--task-id", "MGIT-283.NS3", "--include-uncommitted")

	require.NoError(t, err)
	_, statErr := os.Stat(filepath.Join(wt, "archive", ".mgit", "payload-one"))
	assert.True(t, os.IsNotExist(statErr))
	assert.Equal(t, "package main\n", readFileOrEmpty(t, filepath.Join(wt, "main.go")))
}
