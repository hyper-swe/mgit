package staging

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// FEAT-6.114.1: by default the guest is given a repository of its own: the
// staged files as ONE base commit, none of the host's history. git status,
// git diff and git rev-parse HEAD work there, and nothing of the host's
// history is in the delivered tree.

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=host", "GIT_AUTHOR_EMAIL=host@example.test",
		"GIT_COMMITTER_NAME=host", "GIT_COMMITTER_EMAIL=host@example.test")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
	return strings.TrimSpace(string(out))
}

// hostRepoWithHistory is a worktree with three commits whose subjects are
// recognisable, so a leak is found by text.
func hostRepoWithHistory(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	wt := t.TempDir()
	gitIn(t, wt, "init", "-q", "-b", "main")
	for i, subject := range []string{"HOST-HISTORY-ONE", "HOST-HISTORY-TWO", "HOST-HISTORY-THREE"} {
		require.NoError(t, os.WriteFile(filepath.Join(wt, "f.txt"), []byte(strings.Repeat("x", i+1)+"\n"), 0o644))
		gitIn(t, wt, "add", "-A")
		gitIn(t, wt, "commit", "-q", "-m", subject)
	}
	require.NoError(t, os.WriteFile(filepath.Join(wt, "g.txt"), []byte("untracked work\n"), 0o644))
	return wt
}

func TestTheGuestGetsAFreshRepoWithOneBaseCommit(t *testing.T) {
	wt := hostRepoWithHistory(t)
	stage := filepath.Join(t.TempDir(), "stage")
	require.NoError(t, BuildForGuest(wt, privateStoreWith(t), stage, true))

	assert.Equal(t, "1", gitIn(t, stage, "rev-list", "--count", "HEAD"), "exactly one commit")
	assert.Regexp(t, `^[0-9a-f]{40}$`, gitIn(t, stage, "rev-parse", "HEAD"))
	files := gitIn(t, stage, "ls-files")
	assert.Contains(t, files, "f.txt")
	assert.Contains(t, files, "g.txt", "the staged files, tracked or not on the host, are the base")
	assert.NotContains(t, files, ".mgit", "the private store is not part of the base")
	assert.Empty(t, gitIn(t, stage, "status", "--porcelain"), "the guest starts clean")
	gitIn(t, stage, "diff", "--exit-code")

	// a change in the guest shows up
	require.NoError(t, os.WriteFile(filepath.Join(stage, "f.txt"), []byte("changed\n"), 0o644))
	assert.Contains(t, gitIn(t, stage, "diff", "--stat"), "f.txt")
}

func TestTheGuestRepoCarriesNoHostHistory(t *testing.T) {
	wt := hostRepoWithHistory(t)
	stage := filepath.Join(t.TempDir(), "stage")
	require.NoError(t, BuildForGuest(wt, privateStoreWith(t), stage, true))

	assert.NotContains(t, gitIn(t, stage, "log", "--format=%an %ae %s"), "HOST-HISTORY")
	var all strings.Builder
	require.NoError(t, filepath.WalkDir(filepath.Join(stage, ".git"), func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, _ := os.ReadFile(p)
		all.Write(raw)
		return nil
	}))
	for _, leak := range []string{"HOST-HISTORY", "host@example.test"} {
		assert.NotContains(t, all.String(), leak, "nothing of the host's history is in the delivered repo")
	}
	assert.Equal(t, "1", gitIn(t, stage, "rev-list", "--all", "--count"))
}

func TestWithTheFlagOffTheGuestGetsNoRepo(t *testing.T) {
	wt := hostRepoWithHistory(t)
	stage := filepath.Join(t.TempDir(), "stage")
	require.NoError(t, BuildForGuest(wt, privateStoreWith(t), stage, false))
	_, err := os.Lstat(filepath.Join(stage, ".git"))
	assert.True(t, os.IsNotExist(err), "no .git when the repo is off")
}

func TestARepoIsMadeEvenWhenTheWorktreeHasNoGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	wt := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(wt, "a.txt"), []byte("a\n"), 0o644))
	stage := filepath.Join(t.TempDir(), "stage")
	require.NoError(t, BuildForGuest(wt, privateStoreWith(t), stage, true))
	assert.Equal(t, "1", gitIn(t, stage, "rev-list", "--count", "HEAD"))
}

// The host's git configuration, hooks and templates play no part: the base
// commit is made the same way on every machine.
func TestTheBaseCommitIgnoresHostGitConfigAndHooks(t *testing.T) {
	wt := hostRepoWithHistory(t)
	home := t.TempDir()
	hooks := filepath.Join(home, "hooks")
	require.NoError(t, os.MkdirAll(hooks, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(hooks, "pre-commit"), []byte("#!/bin/sh\nexit 1\n"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(home, ".gitconfig"),
		[]byte("[core]\n\thooksPath = "+hooks+"\n[commit]\n\tgpgsign = true\n[user]\n\tname = leaked-name\n"), 0o644))
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	stage := filepath.Join(t.TempDir(), "stage")
	require.NoError(t, BuildForGuest(wt, privateStoreWith(t), stage, true))
	assert.NotContains(t, gitIn(t, stage, "log", "--format=%an %cn"), "leaked-name")
}

func TestAMissingGitIsSaidNotSilentlySkipped(t *testing.T) {
	wt := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(wt, "a.txt"), []byte("a\n"), 0o644))
	t.Setenv("PATH", t.TempDir())
	stage := filepath.Join(t.TempDir(), "stage")
	err := BuildForGuest(wt, privateStoreWith(t), stage, true)
	require.ErrorIs(t, err, ErrNoGit)
	_, serr := os.Lstat(filepath.Join(stage, "a.txt"))
	assert.NoError(t, serr, "the files were still delivered")
}
