package main

import (
	"bytes"
	"io/fs"
	"os"
	"os/exec"
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

// The pre-push entry point reads developer Git trees, not only mgit's store.
// Stock maintenance packs must work for normal and linked worktrees.
// Refs: FEAT-3.153
func TestRun_StockMaintenancePacks(t *testing.T) {
	for _, linked := range []bool{false, true} {
		name := "normal"
		if linked {
			name = "linked"
		}
		t.Run(name, func(t *testing.T) {
			root := incidentClone(t)
			gitRun := func(args ...string) {
				t.Helper()
				output, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput() //nolint:gosec // fixed Git verbs, test-owned fixture
				require.NoError(t, err, "%s", output)
			}
			path := root
			args := []string{"--repo", path, "--branch", "fix/other-task"}
			if linked {
				path = filepath.Join(t.TempDir(), "linked")
				gitRun("worktree", "add", path, "fix/other-task")
				args = []string{"--repo", path}
			}
			gitRun("maintenance", "run", "--task=loose-objects")
			gitRun("maintenance", "run", "--task=loose-objects")
			gitRun("fsck", "--full")
			packs, err := filepath.Glob(filepath.Join(root, ".git", "objects", "pack", "loose-*.pack"))
			require.NoError(t, err)
			require.NotEmpty(t, packs)
			before := branchguardGitSnapshot(t, root)
			var out, errOut bytes.Buffer
			code := run(args, &out, &errOut)
			require.Equal(t, 0, code, "%s", errOut.String())
			require.Empty(t, errOut.String())
			errOut.Reset()
			code = run([]string{"--repo", path, "--branch", "fix/ci-retry"}, &out, &errOut)
			require.Equal(t, exitRefused, code, "%s", errOut.String())
			require.Contains(t, errOut.String(), "classifier.go")
			require.Equal(t, before, branchguardGitSnapshot(t, root), "guard reads must not change Git bytes")
		})
	}
}

func branchguardGitSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	dotGit := filepath.Join(root, ".git")
	result := map[string]string{}
	require.NoError(t, filepath.WalkDir(dotGit, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(dotGit, path)
		if err != nil {
			return err
		}
		content, err := os.ReadFile(path) //nolint:gosec // path from test-owned Git snapshot walk
		if err != nil {
			return err
		}
		result[relative] = string(content)
		return nil
	}))
	return result
}
