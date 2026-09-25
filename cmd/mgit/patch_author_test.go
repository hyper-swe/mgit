package main

import (
	"os"
	"path/filepath"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// AN EXPORTED PATCH IS AUTHORED BY THE PERSON WHO EXPORTS IT (MGIT-237).
// `git am` records a patch's From: line as the commit's author. The patch
// named mgit's internal squash identity there (`mgit-squash
// <mgit-squash@mgit.local>`), so every commit landed that way carried the
// tool's name in its author field. The From: line now carries the exporter's
// git identity, with git's own precedence: GIT_AUTHOR_NAME/GIT_AUTHOR_EMAIL,
// then the project's git config, then the global one. No identity is a
// refusal, with no patch written. mgit's own store keeps its internal author.
// Refs: MGIT-237

// isolateGitIdentity removes every identity source the test does not set:
// the environment variables, and a global config under a fresh HOME.
func isolateGitIdentity(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, v := range []string{"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL"} {
		t.Setenv(v, "") // registers the restore of the package-wide value
		require.NoError(t, os.Unsetenv(v))
	}
}

// exportedPatches returns the three ways a task's patch leaves mgit: export
// --format git, the squash --to-git preview (--dry-run), and a real squash
// --to-git, each read back as text.
func exportedPatches(t *testing.T, taskID string) map[string]string {
	t.Helper()
	export, _, err := runCLICap(t, "export", "--task-id", taskID, "--format", "git")
	require.NoError(t, err, "export --format git")
	preview, _, err := runCLICap(t, "squash", "--task-id", taskID, "--to-git", "--dry-run")
	require.NoError(t, err, "squash --to-git --dry-run")
	out := filepath.Join(t.TempDir(), "squash.patch")
	require.NoError(t, runCLI(t, "squash", "--task-id", taskID, "--to-git", "--to-git-output", out))
	squash, err := os.ReadFile(out) //nolint:gosec // test-controlled path
	require.NoError(t, err)
	return map[string]string{"export --format git": export, "squash --to-git --dry-run": preview, "squash --to-git": string(squash)}
}

func TestExportedPatch_CarriesTheExportersGitIdentity(t *testing.T) {
	isolateGitIdentity(t)
	t.Setenv("GIT_AUTHOR_NAME", "Ada Lovelace")
	t.Setenv("GIT_AUTHOR_EMAIL", "ada@example.com")
	seedTaskForSquash(t, "WI-7")

	for name, patch := range exportedPatches(t, "WI-7") {
		assert.Contains(t, patch, "\nFrom: Ada Lovelace <ada@example.com>\n", "%s is authored by the exporter", name)
		assert.NotContains(t, patch, "mgit-squash", "%s names mgit's internal identity", name)
		assert.NotContains(t, patch, "mgit.local", "%s names mgit's internal address", name)
	}
}

// The environment outranks the project's git config, and the project's
// config is used when the environment says nothing, as git does it.
func TestExportedPatch_TheEnvironmentOutranksTheProjectsGitConfig(t *testing.T) {
	isolateGitIdentity(t)
	dir := projectWithGit(t)
	repo, err := gogit.PlainOpen(dir)
	require.NoError(t, err)
	cfg, err := repo.Config()
	require.NoError(t, err)
	cfg.User.Name, cfg.User.Email = "Grace Hopper", "grace@example.com"
	require.NoError(t, repo.SetConfig(cfg))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "work.txt"), []byte("work\n"), 0o600))
	require.NoError(t, runCLI(t, "commit", "-a", "--task-id", "WI-8", "-m", "the work"))

	fromConfig, _, err := runCLICap(t, "export", "--task-id", "WI-8", "--format", "git")
	require.NoError(t, err)
	assert.Contains(t, fromConfig, "\nFrom: Grace Hopper <grace@example.com>\n", "the project's git config names the author")

	t.Setenv("GIT_AUTHOR_NAME", "Ada Lovelace")
	t.Setenv("GIT_AUTHOR_EMAIL", "ada@example.com")
	fromEnv, _, err := runCLICap(t, "export", "--task-id", "WI-8", "--format", "git")
	require.NoError(t, err)
	assert.Contains(t, fromEnv, "\nFrom: Ada Lovelace <ada@example.com>\n", "the environment outranks the git config")
}

// No identity anywhere: the patch is refused, named by git's own settings,
// and nothing is written — neither the patch file nor a squash commit.
func TestExportedPatch_NoIdentity_RefusedAndNothingWritten(t *testing.T) {
	isolateGitIdentity(t)
	seedTaskForSquash(t, "WI-9")
	// A squash writes its commit to the task branch task/WI-9, which `mgit
	// log` on main never shows; the branch list is where a squash appears.
	branchesBefore, _, err := runCLICap(t, "branch")
	require.NoError(t, err)
	require.NotContains(t, branchesBefore, "task/WI-9", "no squash has run yet")

	out := filepath.Join(t.TempDir(), "squash.patch")
	err = runCLI(t, "squash", "--task-id", "WI-9", "--to-git", "--to-git-output", out)
	require.Error(t, err, "squash --to-git with no identity is refused")
	assert.Contains(t, err.Error(), "user.name")
	assert.Contains(t, err.Error(), "user.email")
	_, statErr := os.Stat(out)
	assert.True(t, os.IsNotExist(statErr), "no patch is written")
	branchesAfter, _, err := runCLICap(t, "branch")
	require.NoError(t, err)
	assert.Equal(t, branchesBefore, branchesAfter, "the refusal comes before the squash commit, so no task branch is made")

	_, _, err = runCLICap(t, "export", "--task-id", "WI-9", "--format", "git")
	require.Error(t, err, "export --format git with no identity is refused")
	assert.Contains(t, err.Error(), "user.name")
}
