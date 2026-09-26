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

// OPTION C (the founder's ruling on MGIT-237): no identity anywhere refuses
// only the command that WRITES, `squash --to-git`, and it refuses before the
// squash commit, so nothing is written: no patch file, no task branch, and the
// task's micro-commits untouched. The refusal says how to set an identity.
func TestSquashToGit_NoIdentity_RefusedAndNothingWritten(t *testing.T) {
	isolateGitIdentity(t)
	seedTaskForSquash(t, "WI-9")
	branchesBefore, _, err := runCLICap(t, "branch")
	require.NoError(t, err)
	logBefore, _, err := runCLICap(t, "log", "--task-id", "WI-9")
	require.NoError(t, err)

	out := filepath.Join(t.TempDir(), "squash.patch")
	err = runCLI(t, "squash", "--task-id", "WI-9", "--to-git", "--to-git-output", out)

	require.Error(t, err, "squash --to-git with no identity is refused")
	assert.Contains(t, err.Error(), "git config user.name")
	assert.Contains(t, err.Error(), "git config user.email")
	_, statErr := os.Stat(out)
	assert.True(t, os.IsNotExist(statErr), "no patch is written")
	branchesAfter, _, err := runCLICap(t, "branch")
	require.NoError(t, err)
	assert.Equal(t, branchesBefore, branchesAfter, "no squash commit and no task branch")
	logAfter, _, err := runCLICap(t, "log", "--task-id", "WI-9")
	require.NoError(t, err)
	assert.Equal(t, logBefore, logAfter, "the task's micro-commits are untouched")
}

// The read-only paths still work with no identity: they warn, and they name
// NO author at all, neither mgit's internal one nor an invented one. The
// patch carries no From: header, so `git apply` takes it and `git am` asks
// for an author rather than recording a made-up one.
func TestReadOnlyPatch_NoIdentity_WarnsAndNamesNoAuthor(t *testing.T) {
	isolateGitIdentity(t)
	seedTaskForSquash(t, "WI-10")

	for name, args := range map[string][]string{
		"export --format git":       {"export", "--task-id", "WI-10", "--format", "git"},
		"squash --to-git --dry-run": {"squash", "--task-id", "WI-10", "--to-git", "--dry-run"},
	} {
		patch, stderr, err := runCLICap(t, args...)
		require.NoError(t, err, "%s works with no identity", name)
		assert.Contains(t, stderr, "no git identity", "%s warns", name)
		assert.Contains(t, stderr, "git config user.name", "%s says how to set one", name)
		assert.Contains(t, patch, "alpha.txt", "%s still carries the work", name)
		assert.NotRegexp(t, `(?m)^From: `, patch, "%s names no author", name)
		assert.NotContains(t, patch, "mgit-squash", "%s names mgit's internal identity", name)
		assert.NotContains(t, patch, "mgit.local", "%s names mgit's internal address", name)
	}
}

// RECOVERY (the founder's condition): after the refusal, configure an identity
// and re-run the SAME command. It completes with all of the task's work, the
// refused attempt left nothing behind to trip over, and the patch is authored
// by the identity just configured.
func TestSquashToGit_RefusedThenConfigured_CompletesWithAllWork(t *testing.T) {
	isolateGitIdentity(t)
	seedTaskForSquash(t, "WI-11")
	out := filepath.Join(t.TempDir(), "squash.patch")
	args := []string{"squash", "--task-id", "WI-11", "--to-git", "--to-git-output", out}
	require.Error(t, runCLI(t, args...), "refused while no identity is configured")

	home := os.Getenv("HOME")
	require.NoError(t, os.WriteFile(filepath.Join(home, ".gitconfig"),
		[]byte("[user]\n\tname = Grace Hopper\n\temail = grace@example.com\n"), 0o600))
	require.NoError(t, runCLI(t, args...), "the same command completes once an identity is set")

	patch, err := os.ReadFile(out) //nolint:gosec // test-controlled path
	require.NoError(t, err)
	assert.Contains(t, string(patch), "\nFrom: Grace Hopper <grace@example.com>\n", "authored by the configured identity")
	for _, work := range []string{"alpha.txt", "+alpha", "beta.txt", "+beta"} {
		assert.Contains(t, string(patch), work, "all of the task's work is in the patch")
	}
}
