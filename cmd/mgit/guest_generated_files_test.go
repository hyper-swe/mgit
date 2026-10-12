package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/agentadapter"
	"github.com/hyper-swe/mgit/internal/sandboxd/provision"
	"github.com/hyper-swe/mgit/internal/sandboxd/staging"
	gitstore "github.com/hyper-swe/mgit/internal/store/git"
)

// MGIT'S OWN FILES DO NOT LEAVE THROUGH A GUEST COMMIT (MGIT-236). `mgit work
// --sandbox` writes seven agent files into the worktree and records them in
// <worktree>/.mgit/generated; the host's bulk staging skips them (MGIT-80).
// Inside the sandbox that .mgit is the private store, which never received
// the list: a guest `mgit add -A` staged every generated file, they landed,
// and `squash --to-git` and `export --format git` both put mgit's scaffolding
// into the user's patch.
//
// The tree below is the one a backend delivers (provision the private store
// for the worktree, then stage), with the worktree's scaffolding written by
// the same functions `mgit work` runs. The guest stages the way the
// reproduction did, commits, and both patches are read in the guest: land
// imports the guest's commits object for object, so the host's patches are
// built from these same trees. The host-side run after a real land is the
// ticket's live receipt. Refs: MGIT-236, MGIT-80, SEC-03
func TestGuest_BulkStagingLeavesOutTheFilesMgitGenerated(t *testing.T) {
	const taskID = "MGIT-236"
	hostRepo := hostRepoWithCommit(t, taskID)
	worktree := filepath.Join(t.TempDir(), "wt")
	require.NoError(t, runCLI(t, "worktree", "add", worktree, "--task-id", taskID))
	generated := scaffoldLikeWorkSandbox(t, worktree)

	guestTree := deliverWorktreeToGuest(t, hostRepo, worktree, taskID)
	t.Setenv(guestModeEnv, "1")
	t.Chdir(guestTree)
	require.NoError(t, os.WriteFile(filepath.Join(guestTree, "user-work.txt"), []byte("the task's work\n"), 0o600))
	require.NoError(t, runCLI(t, "add", "-A"))
	require.NoError(t, runCLI(t, "commit", "-m", "guest work", "--task-id", taskID))

	squashPatch, _, err := runCLICap(t, "squash", "--task-id", taskID, "--to-git")
	require.NoError(t, err)
	exportPatch, _, err := runCLICap(t, "export", "--task-id", taskID, "--format", "git")
	require.NoError(t, err)
	for name, patch := range map[string]string{"squash --to-git": squashPatch, "export --format git": exportPatch} {
		files := patchFiles(patch)
		assert.Contains(t, files, "user-work.txt", "%s carries the task's own work", name)
		for _, rel := range generated {
			assert.NotContains(t, files, rel, "%s carries %s, a file mgit generated", name, rel)
		}
	}
}

// patchDiffHeader is the header git's patch format gives every changed file.
var patchDiffHeader = regexp.MustCompile(`(?m)^diff --git a/(\S+) b/`)

// patchFiles lists the files a git-format patch changes, read from its
// per-file headers, so a failure names files instead of printing the patch.
func patchFiles(patch string) []string {
	matches := patchDiffHeader.FindAllStringSubmatch(patch, -1)
	files := make([]string, 0, len(matches))
	for _, m := range matches {
		files = append(files, m[1])
	}
	return files
}

// MGIT-80's rule holds in the guest as on the host: a generated file staged
// BY NAME is a statement of intent and lands. The exclusion is bulk staging's
// alone, so carrying the list cannot hide a file the agent means to commit.
// Refs: MGIT-236, MGIT-80
func TestGuest_AGeneratedFileStagedByNameStillLands(t *testing.T) {
	const taskID = "MGIT-236"
	hostRepo := hostRepoWithCommit(t, taskID)
	worktree := filepath.Join(t.TempDir(), "wt")
	require.NoError(t, runCLI(t, "worktree", "add", worktree, "--task-id", taskID))
	require.Contains(t, scaffoldLikeWorkSandbox(t, worktree), "AGENTS.md")

	guestTree := deliverWorktreeToGuest(t, hostRepo, worktree, taskID)
	t.Setenv(guestModeEnv, "1")
	t.Chdir(guestTree)
	require.NoError(t, runCLI(t, "add", "AGENTS.md"))
	require.NoError(t, runCLI(t, "commit", "-m", "keep the agents file", "--task-id", taskID))

	patch, _, err := runCLICap(t, "squash", "--task-id", taskID, "--to-git")
	require.NoError(t, err)
	assert.Equal(t, []string{"AGENTS.md"}, patchFiles(patch), "a file staged by name lands, and only it")
}

// scaffoldLikeWorkSandbox writes the agent files `mgit work --sandbox` writes
// (the contained posture) and records them the way it does, returning the
// recorded list. The list is read back from the worktree, so the assertions
// use what mgit recorded rather than a copy the test keeps.
func scaffoldLikeWorkSandbox(t *testing.T, worktree string) []string {
	t.Helper()
	var warn bytes.Buffer
	injectAgentAdapters(&warn, worktree, true)
	require.NoError(t, upsertWorktreeEnvDoc(worktree, agentadapter.ContainmentPending))
	recordGeneratedScaffolding(&warn, workDeps{recordGenerated: gitstore.RecordGeneratedPaths}, worktree, true)
	require.Empty(t, warn.String(), "scaffolding wrote cleanly")
	generated, err := gitstore.ReadGeneratedPaths(worktree)
	require.NoError(t, err)
	require.Contains(t, generated, "AGENTS.md", "the reproduction's file is among those recorded")
	require.Contains(t, generated, ".claude/settings.json")
	return generated
}

// deliverWorktreeToGuest builds the tree a backend delivers for this
// worktree: the private store provisioned for it, and the worktree staged
// with that store at .mgit. Returns the guest-side worktree root.
func deliverWorktreeToGuest(t *testing.T, hostRepo, worktree, taskID string) string {
	t.Helper()
	prov, err := provision.NewStoreProvisioner(hostRepo)
	require.NoError(t, err)
	store, err := prov.Provision(taskID, worktree, filepath.Join(t.TempDir(), "private-store"))
	require.NoError(t, err)
	guestTree := filepath.Join(t.TempDir(), "staged")
	require.NoError(t, staging.Build(worktree, store.Dir, guestTree))
	return guestTree
}
