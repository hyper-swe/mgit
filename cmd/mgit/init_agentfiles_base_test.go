package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/docs"
)

var agentFiles = []string{"MGIT.md", "CLAUDE.md", "AGENTS.md"}

// The files init writes for agents are part of the base, so the first task's
// status and patch carry only that task's change and nothing has to be
// committed first. Refs: FEAT-6.116
func TestInitRecordsTheAgentFilesInTheBase(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("readme\n"), 0o600))
	t.Chdir(dir)
	require.NoError(t, runCLI(t, "init"))
	for _, f := range agentFiles {
		_, err := os.Stat(filepath.Join(dir, f))
		require.NoError(t, err, "init wrote %s", f)
	}

	status, err := runCLIOut(t, "status")
	require.NoError(t, err)
	for _, f := range agentFiles {
		assert.NotContains(t, status, f, "%s is in the base, not a pending change", f)
	}

	require.NoError(t, os.WriteFile(filepath.Join(dir, "change.txt"), []byte("change\n"), 0o600))
	require.NoError(t, runCLI(t, "commit", "-a", "--task-id=PROJ-1", "-m", "change"))
	patchPath := filepath.Join(t.TempDir(), "task.patch")
	require.NoError(t, runCLI(t, "squash", "--task-id=PROJ-1", "-m", "change", "--to-git", "--to-git-output", patchPath))
	raw, err := os.ReadFile(patchPath) //nolint:gosec // test fixture path
	require.NoError(t, err)
	assert.Contains(t, string(raw), "change.txt")
	for _, f := range append(agentFiles, "README.md") {
		assert.NotContains(t, string(raw), f, "the first task's patch must not carry %s", f)
	}
}

// A pointer line added to a CLAUDE.md the user already has is in the base too.
func TestInitRecordsAnExistingInstructionFileWithItsPointerInTheBase(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("mine\n"), 0o600))
	t.Chdir(dir)
	require.NoError(t, runCLI(t, "init"))
	got, err := os.ReadFile(filepath.Join(dir, "CLAUDE.md")) //nolint:gosec // test fixture path
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(string(got), "mine\n"), "the user's text stays first")
	assert.Contains(t, string(got), "@MGIT.md")
	status, err := runCLIOut(t, "status")
	require.NoError(t, err)
	assert.NotContains(t, status, "CLAUDE.md")
}

func TestInitWithNoAgentFilesWritesNone(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	require.NoError(t, runCLI(t, "init", "--no-agent-files"))
	for _, f := range agentFiles {
		_, err := os.Stat(filepath.Join(dir, f))
		assert.True(t, os.IsNotExist(err), "%s must not be written", f)
	}
}

// With the files in the base, the documented workflow needs no baseline commit.
func TestAgentInstructionsDoNotAskForABaselineCommit(t *testing.T) {
	assert.NotContains(t, strings.ToLower(docs.AgentInstructions()), "baseline")
}

// init --path into a directory that does not exist yet creates it and writes
// the agent files, as before the files moved ahead of the base.
func TestInitPathToANewDirectoryWritesTheAgentFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "fresh", "project")
	require.NoError(t, runCLI(t, "init", "--path", dir))
	for _, f := range agentFiles {
		_, err := os.Stat(filepath.Join(dir, f))
		assert.NoError(t, err, "%s is written in the new directory", f)
	}
	t.Chdir(dir)
	status, err := runCLIOut(t, "status")
	require.NoError(t, err)
	for _, f := range agentFiles {
		assert.NotContains(t, status, f)
	}
}

// If init fails, the agent files it wrote ahead of the base are taken back:
// files it created are removed, and a CLAUDE.md the project had is restored
// to exactly what it was.
func TestAFailedInitLeavesTheAgentFilesAsItFoundThem(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("an unreadable file stays readable to root")
	}
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("mine"), 0o600)) // no trailing newline
	locked := filepath.Join(dir, "locked.txt")
	require.NoError(t, os.WriteFile(locked, []byte("x\n"), 0o000))
	t.Cleanup(func() { _ = os.Chmod(locked, 0o600) })
	t.Chdir(dir)

	require.Error(t, runCLI(t, "init"), "init cannot read locked.txt")
	got, err := os.ReadFile(filepath.Join(dir, "CLAUDE.md")) //nolint:gosec // test fixture path
	require.NoError(t, err)
	assert.Equal(t, "mine", string(got), "the project's CLAUDE.md is exactly as it was")
	for _, f := range []string{"MGIT.md", "AGENTS.md"} {
		_, statErr := os.Stat(filepath.Join(dir, f))
		assert.True(t, os.IsNotExist(statErr), "%s is removed again", f)
	}
}
