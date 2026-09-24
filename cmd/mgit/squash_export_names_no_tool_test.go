package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// WHAT LEAVES FOR THE USER'S GIT NAMES NO TOOL. mgit tags every commit in its
// own store with `[MGIT:<task>] `. The default squash message summarized each
// micro-commit by copying its message, tag and all, and both ways out,
// `squash --to-git` and `export --format git`, carried that summary into the
// patch, where `git am` records it in the adopter's history (reproduced
// independently on MGIT-228). The store keeps its tag, which is mgit's own
// bookkeeping. What leaves keeps what was written and drops the tag; the task
// stays traceable through mgit's index. Refs: MGIT-228
func TestSquashAndExport_DefaultMessage_CarryNoTaskTag(t *testing.T) {
	const taskID = "WI-2"
	seedTaskForSquash(t, taskID)

	logOut, _, err := runCLICap(t, "log", "--oneline")
	require.NoError(t, err)
	require.Contains(t, logOut, "[MGIT:WI-2] first step", "the store keeps its own tag")

	exportPatch, _, err := runCLICap(t, "export", "--task-id", taskID, "--format", "git")
	require.NoError(t, err)
	patchFile := filepath.Join(t.TempDir(), "squash.patch")
	require.NoError(t, runCLI(t, "squash", "--task-id", taskID, "--to-git", "--to-git-output", patchFile))
	squashPatch, err := os.ReadFile(patchFile) //nolint:gosec // test-controlled path
	require.NoError(t, err)

	for name, patch := range map[string]string{"export --format git": exportPatch, "squash --to-git": string(squashPatch)} {
		assert.NotContains(t, patch, "[MGIT:", "%s carries mgit's task tag toward the user's git", name)
		assert.Contains(t, patch, "first step", "%s keeps what was written", name)
		assert.Contains(t, patch, "second step", "%s keeps what was written", name)
	}
}
