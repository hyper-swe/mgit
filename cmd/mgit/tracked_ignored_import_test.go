package main

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

// FILES GIT TRACKS ARE THE PROJECT'S, WHATEVER THE IGNORE RULES SAY
// (MGIT-269). Git applies ignore rules to UNTRACKED files only: a file
// force-added under a `*.log` rule, or committed inside a directory a later
// `build/` rule ignores, stays tracked. mgit's import walked the working tree
// through the ignore rules alone (MGIT-32), so its base never held those
// files, and neither did any task worktree cut from it. A loop reading the
// worktree saw them as deleted, and a build needing one failed.
//
// The repository below is the reproduction: keep.log and build/tracked.txt are
// committed first, then the .gitignore that covers them, so git tracks both.
// docs/AGENTS.md and docs/codex/AGENTS.md sit under an ANCHORED /AGENTS.md
// rule in .git/info/exclude, which does not reach them: the guard case, green
// before the fix. untracked.log and build/untracked.txt are ignored and not
// tracked: they must stay out. Refs: MGIT-269, MGIT-32
func TestWorktree_CarriesFilesGitTracksDespiteIgnoreRules(t *testing.T) {
	projectTrackingIgnoredFiles(t)
	wt := filepath.Join(t.TempDir(), "wt")

	require.NoError(t, runCLI(t, "worktree", "add", wt, "--task-id", "MGIT-269"))

	tests := []struct {
		name string
		path string
	}{
		{"a file force-added under a *.log rule", "keep.log"},
		{"a file committed inside a directory build/ ignores", "build/tracked.txt"},
		{"guard: a tracked file beside an anchored exclude rule", "docs/AGENTS.md"},
		{"guard: the same name one level deeper", "docs/codex/AGENTS.md"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := os.ReadFile(filepath.Join(wt, filepath.FromSlash(tt.path))) //nolint:gosec // test-controlled path
			require.NoError(t, err, "the task worktree holds %s, which git tracks", tt.path)
			assert.Equal(t, trackedContent(tt.path), string(got))
		})
	}
	for _, untracked := range []string{"untracked.log", "build/untracked.txt"} {
		_, err := os.Stat(filepath.Join(wt, filepath.FromSlash(untracked)))
		assert.True(t, os.IsNotExist(err), "%s is ignored and untracked, so it stays out of the worktree", untracked)
	}
}

// The files git tracks are in mgit's own base tree, not only copied beside it:
// `mgit status` in the project reports none of them deleted, and a tracked
// file's edit is seen. Refs: MGIT-269
func TestStatus_SeesFilesGitTracksDespiteIgnoreRules(t *testing.T) {
	dir := projectTrackingIgnoredFiles(t)
	require.NoError(t, runCLI(t, "worktree", "add", filepath.Join(t.TempDir(), "wt"), "--task-id", "MGIT-269"))

	clean, _, err := runCLICap(t, "status", "--porcelain")
	require.NoError(t, err)
	for _, path := range []string{"keep.log", "build/tracked.txt"} {
		assert.NotContains(t, clean, path, "%s is in the base and unchanged, so status is silent about it", path)
	}

	require.NoError(t, os.WriteFile(filepath.Join(dir, "keep.log"), []byte("edited\n"), 0o600))
	edited, _, err := runCLICap(t, "status", "--porcelain")
	require.NoError(t, err)
	assert.Contains(t, edited, "keep.log", "an edit to a tracked file is seen even though *.log is ignored")
	assert.NotContains(t, edited, "untracked.log", "an untracked ignored file stays unseen")
}

// projectTrackingIgnoredFiles builds the reproduction repository and runs
// mgit init in it.
func projectTrackingIgnoredFiles(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	repo, err := gogit.PlainInit(dir, false)
	require.NoError(t, err)
	wt, err := repo.Worktree()
	require.NoError(t, err)
	sig := &object.Signature{Name: "dev", Email: "dev@x", When: time.Unix(0, 0).UTC()}
	commit := func(msg string, paths ...string) {
		for _, p := range paths {
			abs := filepath.Join(dir, filepath.FromSlash(p))
			require.NoError(t, os.MkdirAll(filepath.Dir(abs), 0o750))
			require.NoError(t, os.WriteFile(abs, []byte(trackedContent(p)), 0o600))
			_, err := wt.Add(p)
			require.NoError(t, err)
		}
		_, err := wt.Commit(msg, &gogit.CommitOptions{Author: sig, Committer: sig})
		require.NoError(t, err)
	}
	commit("files git tracks", "main.go", "keep.log", "build/tracked.txt", "docs/AGENTS.md", "docs/codex/AGENTS.md")
	commit("ignore logs and build output", ".gitignore")
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".git", "info"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".git", "info", "exclude"), []byte("/CLAUDE.md\n/AGENTS.md\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "untracked.log"), []byte("noise\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "build", "untracked.txt"), []byte("output\n"), 0o600))
	t.Chdir(dir)
	require.NoError(t, runCLI(t, "init"))
	return dir
}

// trackedContent is each fixture file's committed content.
func trackedContent(path string) string {
	if path == ".gitignore" {
		return "*.log\nbuild/\n"
	}
	return "content of " + path + "\n"
}
