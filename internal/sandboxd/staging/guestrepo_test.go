package staging

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// FEAT-6.114.1: by default the guest is given a repository of its own: the
// staged files as ONE base commit, none of the host's history. It is made
// in process (go-git), so nothing in the host's git environment reaches it,
// and nothing of the host's history is in the delivered tree.

func commitAll(t *testing.T, dir, subject string) {
	t.Helper()
	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)
	wt, err := repo.Worktree()
	require.NoError(t, err)
	require.NoError(t, wt.AddWithOptions(&git.AddOptions{All: true}))
	sig := &object.Signature{Name: "host", Email: "host@example.test", When: time.Now()}
	_, err = wt.Commit(subject, &git.CommitOptions{Author: sig, Committer: sig, AllowEmptyCommits: true})
	require.NoError(t, err)
}

func commitCount(t *testing.T, dir string) int {
	t.Helper()
	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)
	it, err := repo.Log(&git.LogOptions{All: true})
	if err != nil { // no commits yet
		return 0
	}
	n := 0
	require.NoError(t, it.ForEach(func(*object.Commit) error { n++; return nil }))
	return n
}

// hostRepoWithHistory is a worktree with three commits whose subjects are
// recognisable, so a leak is found by text.
func hostRepoWithHistory(t *testing.T) string {
	t.Helper()
	wt := t.TempDir()
	_, err := git.PlainInit(wt, false)
	require.NoError(t, err)
	for i, subject := range []string{"HOST-HISTORY-ONE", "HOST-HISTORY-TWO", "HOST-HISTORY-THREE"} {
		require.NoError(t, os.WriteFile(filepath.Join(wt, "f.txt"), []byte(strings.Repeat("x", i+1)+"\n"), 0o644))
		commitAll(t, wt, subject)
	}
	require.NoError(t, os.WriteFile(filepath.Join(wt, "g.txt"), []byte("untracked work\n"), 0o644))
	return wt
}

func TestTheGuestGetsAFreshRepoWithOneBaseCommit(t *testing.T) {
	wt := hostRepoWithHistory(t)
	stage := filepath.Join(t.TempDir(), "stage")
	require.NoError(t, BuildForGuest(wt, privateStoreWith(t), stage, true))

	assert.Equal(t, 1, commitCount(t, stage), "exactly one commit")
	repo, err := git.PlainOpen(stage)
	require.NoError(t, err)
	head, err := repo.Head()
	require.NoError(t, err)
	assert.Equal(t, "refs/heads/main", head.Name().String())
	c, err := repo.CommitObject(head.Hash())
	require.NoError(t, err)
	files, err := c.Files()
	require.NoError(t, err)
	var names []string
	require.NoError(t, files.ForEach(func(f *object.File) error { names = append(names, f.Name); return nil }))
	assert.Contains(t, names, "f.txt")
	assert.Contains(t, names, "g.txt", "the staged files, tracked or not on the host, are the base")
	for _, n := range names {
		assert.False(t, strings.HasPrefix(n, ".mgit"), "the private store is not part of the base: %s", n)
		assert.False(t, strings.HasPrefix(n, ".git/"), n)
	}
	// git (not go-git) reads info/exclude, which keeps the store out of status
	ex, err := os.ReadFile(filepath.Join(stage, ".git", "info", "exclude"))
	require.NoError(t, err)
	assert.Contains(t, string(ex), ".mgit/")
}

// With a git binary at hand, the three commands an agent runs work on the
// delivered tree and see an edit.
func TestGitCommandsWorkOnTheDeliveredRepo(t *testing.T) {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed: this test runs the real commands")
	}
	wt := hostRepoWithHistory(t)
	stage := filepath.Join(t.TempDir(), "stage")
	require.NoError(t, BuildForGuest(wt, privateStoreWith(t), stage, true))
	run := func(args ...string) string {
		out, rerr := exec.Command(gitBin, append([]string{"-C", stage}, args...)...).CombinedOutput()
		require.NoError(t, rerr, "git %v: %s", args, out)
		return strings.TrimSpace(string(out))
	}
	assert.Regexp(t, `^[0-9a-f]{40}$`, run("rev-parse", "HEAD"))
	assert.Empty(t, run("status", "--porcelain"))
	run("diff", "--exit-code")
	require.NoError(t, os.WriteFile(filepath.Join(stage, "f.txt"), []byte("changed\n"), 0o644))
	assert.Contains(t, run("diff", "--stat"), "f.txt")
}

func TestTheGuestRepoCarriesNoHostHistory(t *testing.T) {
	wt := hostRepoWithHistory(t)
	stage := filepath.Join(t.TempDir(), "stage")
	require.NoError(t, BuildForGuest(wt, privateStoreWith(t), stage, true))

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
}

func TestWithTheFlagOffTheGuestGetsNoRepo(t *testing.T) {
	wt := hostRepoWithHistory(t)
	stage := filepath.Join(t.TempDir(), "stage")
	require.NoError(t, BuildForGuest(wt, privateStoreWith(t), stage, false))
	_, err := os.Lstat(filepath.Join(stage, ".git"))
	assert.True(t, os.IsNotExist(err), "no .git when the repo is off")
}

func TestARepoIsMadeEvenWhenTheWorktreeHasNoGit(t *testing.T) {
	wt := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(wt, "a.txt"), []byte("a\n"), 0o644))
	stage := filepath.Join(t.TempDir(), "stage")
	require.NoError(t, BuildForGuest(wt, privateStoreWith(t), stage, true))
	assert.Equal(t, 1, commitCount(t, stage))
}

func TestTheBaseCommitIsTheSameForTheSameFiles(t *testing.T) {
	wt := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(wt, "a.txt"), []byte("a\n"), 0o644))
	hash := func() string {
		stage := filepath.Join(t.TempDir(), "stage")
		require.NoError(t, BuildForGuest(wt, privateStoreWith(t), stage, true))
		repo, err := git.PlainOpen(stage)
		require.NoError(t, err)
		h, err := repo.Head()
		require.NoError(t, err)
		return h.Hash().String()
	}
	assert.Equal(t, hash(), hash(), "a fixed identity and a fixed date")
}

// The host's git environment reaches nothing: a launch made from a git hook
// or a `git -C` wrapper has GIT_DIR, GIT_INDEX_FILE and GIT_OBJECT_DIRECTORY
// set, pointing at a host repository. The staged tree's own repository is
// made, and that repository is left exactly as it was.
func TestTheHostsGitEnvironmentReachesNoOtherRepository(t *testing.T) {
	for _, tc := range []struct {
		variable string
		value    func(other string) string
	}{
		{"GIT_DIR", func(o string) string { return filepath.Join(o, ".git") }},
		{"GIT_INDEX_FILE", func(o string) string { return filepath.Join(o, ".git", "index") }},
		{"GIT_OBJECT_DIRECTORY", func(o string) string { return filepath.Join(o, ".git", "objects") }},
	} {
		t.Run(tc.variable, func(t *testing.T) {
			other := hostRepoWithHistory(t)
			before := snapshotDir(t, filepath.Join(other, ".git"))
			t.Setenv(tc.variable, tc.value(other))

			wt := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(wt, "a.txt"), []byte("a\n"), 0o644))
			stage := filepath.Join(t.TempDir(), "stage")
			require.NoError(t, BuildForGuest(wt, privateStoreWith(t), stage, true))

			assert.Equal(t, before, snapshotDir(t, filepath.Join(other, ".git")), "the other repository is untouched")
			assert.Equal(t, 3, commitCount(t, other))
			assert.Equal(t, 1, commitCount(t, stage), "the staged tree has its own base commit")
		})
	}
}

// snapshotDir is every file under dir with its size and content hash input,
// so any write, however small, shows.
func snapshotDir(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	require.NoError(t, filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, rerr := os.ReadFile(p)
		require.NoError(t, rerr)
		rel, _ := filepath.Rel(dir, p)
		out[rel] = string(raw)
		return nil
	}))
	return out
}

func TestAnErrorBuildingTheRepoReachesTheCaller(t *testing.T) {
	wt := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(wt, "a.txt"), []byte("a\n"), 0o644))
	// a file where the repository's directory would go
	stage := filepath.Join(t.TempDir(), "stage")
	require.NoError(t, os.MkdirAll(stage, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(stage, ".git"), []byte("not a directory"), 0o644))
	err := BuildForGuest(wt, privateStoreWith(t), stage, true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--no-guest-repo", "the error names the way out")
}
