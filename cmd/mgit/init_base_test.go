package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// existingTree writes the six-file project the report reproduced with:
// source in a subdirectory, an executable script, a dotfile, and a file the
// project's own ignore rules hide.
func existingTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := map[string]string{
		"README.md":      "readme\n",
		"main.go":        "package main\n",
		"pkg/util.go":    "package pkg\n",
		"scripts/run.sh": "#!/bin/sh\necho run\n",
		".editorconfig":  "root = true\n",
		".gitignore":     "build/\n",
	}
	for rel, body := range files {
		p := filepath.Join(dir, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o750))
		mode := os.FileMode(0o600)
		if rel == "scripts/run.sh" {
			mode = 0o700
		}
		require.NoError(t, os.WriteFile(p, []byte(body), mode))
	}
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "build"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "build", "out.bin"), []byte("ignored\n"), 0o600))
	return files
}

// mgit init over an existing tree records that tree as the base, so the first
// task's patch carries only that task's changes and applies on a checkout that
// already has the files. Refs: FEAT-6.116
func TestInitOverAnExistingTreeRecordsItAsTheBase(t *testing.T) {
	dir := t.TempDir()
	files := existingTree(t, dir)
	t.Chdir(dir)
	require.NoError(t, runCLI(t, "init"))

	// nothing the project already had is a pending change
	status, err := runCLIOut(t, "status")
	require.NoError(t, err)
	assert.NotContains(t, status, "README.md", "the existing tree is the base, not a change")

	require.NoError(t, os.WriteFile(filepath.Join(dir, "pkg", "new.go"), []byte("package pkg\n// new\n"), 0o600))
	require.NoError(t, runCLI(t, "commit", "-a", "--task-id=PROJ-1", "-m", "add new.go"))
	patchPath := filepath.Join(t.TempDir(), "task.patch")
	require.NoError(t, runCLI(t, "squash", "--task-id=PROJ-1", "-m", "add new.go", "--to-git", "--to-git-output", patchPath))

	raw, err := os.ReadFile(patchPath) //nolint:gosec // test fixture path
	require.NoError(t, err)
	patch := string(raw)
	assert.Contains(t, patch, "pkg/new.go")
	for rel := range files {
		assert.NotContains(t, patch, rel, "the patch must not carry a file the project already had")
	}
	assert.NotContains(t, patch, "build/out.bin")

	// a plain checkout that already has the six files takes the patch
	tgt := t.TempDir()
	for rel, body := range files {
		p := filepath.Join(tgt, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o750))
		require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
	}
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"apply", "--check", patchPath}, {"apply", patchPath}} {
		out, err := exec.Command("git", append([]string{"-C", tgt}, args...)...).CombinedOutput() //nolint:gosec // test-controlled arguments
		require.NoError(t, err, "git %v: %s", args, out)
	}
	_, statErr := os.Stat(filepath.Join(tgt, "pkg", "new.go"))
	assert.NoError(t, statErr)
}

// Init in an empty directory records nothing extra: the base is empty.
func TestInitInAnEmptyDirectoryKeepsAnEmptyBase(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	require.NoError(t, runCLI(t, "init"))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\n"), 0o600))
	require.NoError(t, runCLI(t, "commit", "-a", "--task-id=PROJ-1", "-m", "add a"))
	patchPath := filepath.Join(t.TempDir(), "task.patch")
	require.NoError(t, runCLI(t, "squash", "--task-id=PROJ-1", "-m", "add a", "--to-git", "--to-git-output", patchPath))
	raw, err := os.ReadFile(patchPath) //nolint:gosec // test fixture path
	require.NoError(t, err)
	assert.Contains(t, string(raw), "a.txt")
}

// A git checkout's .git directory is never part of the base.
func TestInitOverAGitCheckoutLeavesDotGitOutOfTheBase(t *testing.T) {
	dir := t.TempDir()
	existingTree(t, dir)
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"config", "user.email", "t@e.x"}, {"config", "user.name", "T"}, {"add", "-A"}, {"commit", "-qm", "base"}} {
		out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput() //nolint:gosec // test-controlled arguments
		require.NoError(t, err, "git %v: %s", args, out)
	}
	t.Chdir(dir)
	require.NoError(t, runCLI(t, "init"))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "extra.txt"), []byte("x\n"), 0o600))
	require.NoError(t, runCLI(t, "commit", "-a", "--task-id=PROJ-1", "-m", "extra"))
	patchPath := filepath.Join(t.TempDir(), "task.patch")
	require.NoError(t, runCLI(t, "squash", "--task-id=PROJ-1", "-m", "extra", "--to-git", "--to-git-output", patchPath))
	raw, err := os.ReadFile(patchPath) //nolint:gosec // test fixture path
	require.NoError(t, err)
	assert.Contains(t, string(raw), "extra.txt")
	assert.NotContains(t, string(raw), ".git/")
	assert.NotContains(t, string(raw), "README.md")
}
