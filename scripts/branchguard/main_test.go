package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/require"
)

// incidentClone builds the 9abf4ce shape on disk: a task branch with work of
// its own, and a one-file branch cut from it by `git checkout -b`.
// Refs: MGIT-142
func incidentClone(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	repo, err := gogit.PlainInitWithOptions(dir, &gogit.PlainInitOptions{
		InitOptions: gogit.InitOptions{DefaultBranch: plumbing.Main},
	})
	require.NoError(t, err)
	wt, err := repo.Worktree()
	require.NoError(t, err)
	at := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	commit := func(msg string, files ...string) {
		for _, f := range files {
			require.NoError(t, os.WriteFile(filepath.Join(dir, f), []byte(msg), 0o600))
			_, err := wt.Add(f)
			require.NoError(t, err)
		}
		at = at.Add(time.Minute)
		_, err := wt.Commit(msg, &gogit.CommitOptions{
			Author: &object.Signature{Name: "T", Email: "t@example.com", When: at}})
		require.NoError(t, err)
	}
	branch := func(name string) {
		require.NoError(t, wt.Checkout(&gogit.CheckoutOptions{
			Branch: plumbing.NewBranchReferenceName(name), Create: true}))
	}
	commit("chore: seed", "README.md")
	branch("fix/other-task")
	commit("fix: another task's work", "classifier.go")
	branch("fix/ci-retry")
	commit("ci: retry the build", "build.sh")
	return dir
}

func TestRun_BranchCutFromAnotherBranch_RefusesWithFilesAndOverride(t *testing.T) {
	dir := incidentClone(t)
	var out, errOut bytes.Buffer

	code := run([]string{"--repo", dir}, &out, &errOut)

	require.Equal(t, exitRefused, code)
	require.Contains(t, errOut.String(), "BRANCH SCOPE REFUSED")
	require.Contains(t, errOut.String(), "classifier.go")
	require.Contains(t, errOut.String(), "Branch-Scope-Override:")
	require.Empty(t, out.String(), "a refusal belongs on stderr, where a hook shows it")
}

func TestRun_DeclaredBase_Passes(t *testing.T) {
	dir := incidentClone(t)
	var out, errOut bytes.Buffer

	code := run([]string{"--repo", dir, "--base", "fix/other-task"}, &out, &errOut)

	require.Equal(t, 0, code)
	require.Empty(t, errOut.String(), "a clean branch says nothing at push time")
}

func TestRun_CleanBranch_Silent(t *testing.T) {
	dir := incidentClone(t)
	var out, errOut bytes.Buffer

	code := run([]string{"--repo", dir, "--branch", "fix/other-task"}, &out, &errOut)

	require.Equal(t, 0, code)
	require.Empty(t, errOut.String())
}

func TestRun_Survey_ReportsRefusedCount(t *testing.T) {
	dir := incidentClone(t)
	var out, errOut bytes.Buffer

	code := run([]string{"--repo", dir, "--survey"}, &out, &errOut)

	require.Equal(t, 0, code)
	require.Contains(t, out.String(), "REFUSED  fix/ci-retry")
	require.Contains(t, out.String(), "2 branches surveyed against main+origin/main, 1 refused")
}

func TestRun_UnknownRepository_ReturnsError(t *testing.T) {
	var out, errOut bytes.Buffer

	code := run([]string{"--repo", filepath.Join(t.TempDir(), "nowhere")}, &out, &errOut)

	require.Equal(t, exitError, code)
	require.Contains(t, errOut.String(), "branchguard:")
}

func TestRun_UnknownFlag_ReturnsError(t *testing.T) {
	var out, errOut bytes.Buffer

	code := run([]string{"--no-such-flag"}, &out, &errOut)

	require.Equal(t, exitError, code)
}

func TestRun_UnknownBranch_ReturnsError(t *testing.T) {
	dir := incidentClone(t)
	var out, errOut bytes.Buffer

	code := run([]string{"--repo", dir, "--branch", "no/such/branch"}, &out, &errOut)

	require.Equal(t, exitError, code)
	require.Contains(t, errOut.String(), "no/such/branch")
}
