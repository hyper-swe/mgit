package gitref

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// commitRealGitRepo creates a real git repo at dir with one commit and returns
// the commit hash. It uses go-git (no shelling out, per CLAUDE.md).
func commitRealGitRepo(t *testing.T, dir string) string {
	t.Helper()
	repo, err := gogit.PlainInit(dir, false)
	require.NoError(t, err)
	wt, err := repo.Worktree()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o600))
	_, err = wt.Add("main.go")
	require.NoError(t, err)
	sig := &object.Signature{Name: "dev", Email: "dev@x"}
	h, err := wt.Commit("initial", &gogit.CommitOptions{Author: sig, Committer: sig})
	require.NoError(t, err)
	return h.String()
}

// snapshotDir returns a map of relative path -> bytes for every file under root,
// used to prove gitref never mutates `.git`.
func snapshotDir(t *testing.T, root string) map[string]string {
	t.Helper()
	snap := make(map[string]string)
	err := filepath.Walk(root, func(p string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		require.NoError(t, err)
		data, err := os.ReadFile(p) //nolint:gosec // test path
		require.NoError(t, err)
		snap[rel] = string(data)
		return nil
	})
	require.NoError(t, err)
	return snap
}

func TestReadLocalState_RealGitDir_ResolvesHead(t *testing.T) {
	dir := t.TempDir()
	want := commitRealGitRepo(t, dir)

	st, err := ReadLocalState(dir)
	require.NoError(t, err)
	assert.Equal(t, want, st.HeadCommit)
	assert.Equal(t, "refs/heads/master", st.Ref)
}

func TestReadLocalState_ReadOnly_NeverMutatesGit(t *testing.T) {
	dir := t.TempDir()
	commitRealGitRepo(t, dir)
	gitDir := filepath.Join(dir, ".git")

	before := snapshotDir(t, gitDir)
	_, err := ReadLocalState(dir)
	require.NoError(t, err)
	after := snapshotDir(t, gitDir)

	assert.Equal(t, before, after, "gitref must never mutate the project .git")
}

func TestReadLocalState_GitFileGitdirPointer_ResolvesHead(t *testing.T) {
	// Simulate a linked git worktree / submodule: `.git` is a FILE whose
	// content is `gitdir: <path>` pointing at the real git directory.
	real := t.TempDir()
	want := commitRealGitRepo(t, real)
	realGitDir := filepath.Join(real, ".git")

	worktree := t.TempDir()
	// Point the worktree's `.git` FILE at the real git dir (absolute pointer).
	require.NoError(t, os.WriteFile(filepath.Join(worktree, ".git"),
		[]byte("gitdir: "+realGitDir+"\n"), 0o600))

	st, err := ReadLocalState(worktree)
	require.NoError(t, err)
	assert.Equal(t, want, st.HeadCommit)
}

func TestReadLocalState_RelativeGitdirPointer_ResolvesHead(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "realrepo")
	require.NoError(t, os.MkdirAll(real, 0o750))
	want := commitRealGitRepo(t, real)

	worktree := filepath.Join(root, "wt")
	require.NoError(t, os.MkdirAll(worktree, 0o750))
	// Relative pointer resolved against the worktree root.
	require.NoError(t, os.WriteFile(filepath.Join(worktree, ".git"),
		[]byte("gitdir: ../realrepo/.git\n"), 0o600))

	st, err := ReadLocalState(worktree)
	require.NoError(t, err)
	assert.Equal(t, want, st.HeadCommit)
}

func TestReadLocalState_SymlinkedGit_ResolvesHead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}
	real := t.TempDir()
	want := commitRealGitRepo(t, real)
	realGitDir := filepath.Join(real, ".git")

	worktree := t.TempDir()
	require.NoError(t, os.Symlink(realGitDir, filepath.Join(worktree, ".git")))

	st, err := ReadLocalState(worktree)
	require.NoError(t, err)
	assert.Equal(t, want, st.HeadCommit)
}

func TestReadLocalState_PackedRefsOnly_ResolvesHead(t *testing.T) {
	dir := t.TempDir()
	want := commitRealGitRepo(t, dir)
	gitDir := filepath.Join(dir, ".git")

	// Move the loose ref into packed-refs to exercise the packed fallback.
	loose := filepath.Join(gitDir, "refs", "heads", "master")
	require.NoError(t, os.Remove(loose))
	require.NoError(t, os.WriteFile(filepath.Join(gitDir, "packed-refs"),
		[]byte("# pack-refs with: peeled fully-peeled sorted\n"+want+" refs/heads/master\n"), 0o600))

	st, err := ReadLocalState(dir)
	require.NoError(t, err)
	assert.Equal(t, want, st.HeadCommit)
}

func TestReadLocalState_NoGit_ReturnsErrNoGit(t *testing.T) {
	dir := t.TempDir()
	_, err := ReadLocalState(dir)
	assert.ErrorIs(t, err, ErrNoGit)
}

func TestReadLocalState_ShallowClone_FailsLoud(t *testing.T) {
	dir := t.TempDir()
	commitRealGitRepo(t, dir)
	// A `shallow` marker file signals a shallow clone.
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".git", "shallow"),
		[]byte("0000000000000000000000000000000000000000\n"), 0o600))

	_, err := ReadLocalState(dir)
	assert.ErrorIs(t, err, ErrUnsupportedGitState)
}

func TestReadLocalState_SparseCheckout_FailsLoud(t *testing.T) {
	dir := t.TempDir()
	commitRealGitRepo(t, dir)
	cfgPath := filepath.Join(dir, ".git", "config")
	data, err := os.ReadFile(cfgPath) //nolint:gosec // test path
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(cfgPath, //nolint:gosec // test path
		append(data, []byte("[core]\n\tsparseCheckout = true\n")...), 0o600))

	_, err = ReadLocalState(dir)
	assert.ErrorIs(t, err, ErrUnsupportedGitState)
}

func TestReadLocalState_UnbornBranch_ReturnsDetachedOrUnborn(t *testing.T) {
	dir := t.TempDir()
	_, err := gogit.PlainInit(dir, false)
	require.NoError(t, err)
	// Fresh init: HEAD points at refs/heads/master which has no commit yet.
	_, err = ReadLocalState(dir)
	assert.True(t, errors.Is(err, ErrDetachedOrUnborn) || errors.Is(err, ErrNoGit),
		"unborn branch must fail loud, got %v", err)
}

func TestReadLocalState_ChainedSymref_ResolvesHead(t *testing.T) {
	dir := t.TempDir()
	want := commitRealGitRepo(t, dir)
	gitDir := filepath.Join(dir, ".git")

	// Make refs/heads/master a SYMBOLIC ref to another branch holding the commit
	// (a legal chained symref). A naive reader treats the "ref:" content as
	// "not a commit id" → false unborn → silent stale base; we must follow it.
	require.NoError(t, os.WriteFile(filepath.Join(gitDir, "refs", "heads", "real"),
		[]byte(want+"\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(gitDir, "refs", "heads", "master"),
		[]byte("ref: refs/heads/real\n"), 0o600))

	st, err := ReadLocalState(dir)
	require.NoError(t, err)
	assert.Equal(t, want, st.HeadCommit)
}

func TestReadLocalState_LinkedWorktreeCommondir_ResolvesSharedRef(t *testing.T) {
	real := t.TempDir()
	want := commitRealGitRepo(t, real)
	realGitDir := filepath.Join(real, ".git")
	// A per-worktree gitdir with a `commondir` pointer and its own HEAD pointing
	// at a SHARED branch whose loose ref lives in the COMMON dir.
	perWT := filepath.Join(realGitDir, "worktrees", "wt1")
	require.NoError(t, os.MkdirAll(perWT, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(perWT, "commondir"), []byte("../..\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(perWT, "HEAD"),
		[]byte("ref: refs/heads/master\n"), 0o600))

	worktree := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(worktree, ".git"),
		[]byte("gitdir: "+perWT+"\n"), 0o600))

	st, err := ReadLocalState(worktree)
	require.NoError(t, err)
	assert.Equal(t, want, st.HeadCommit)
}

func TestReadLocalState_ShallowInCommondir_FromLinkedWorktree_FailsLoud(t *testing.T) {
	real := t.TempDir()
	commitRealGitRepo(t, real)
	realGitDir := filepath.Join(real, ".git")
	perWT := filepath.Join(realGitDir, "worktrees", "wt1")
	require.NoError(t, os.MkdirAll(perWT, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(perWT, "commondir"), []byte("../..\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(perWT, "HEAD"),
		[]byte("ref: refs/heads/master\n"), 0o600))
	// The shallow marker lives in the COMMON dir, not the per-worktree gitdir.
	require.NoError(t, os.WriteFile(filepath.Join(realGitDir, "shallow"),
		[]byte("0000000000000000000000000000000000000000\n"), 0o600))

	worktree := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(worktree, ".git"),
		[]byte("gitdir: "+perWT+"\n"), 0o600))

	_, err := ReadLocalState(worktree)
	assert.ErrorIs(t, err, ErrUnsupportedGitState)
}

func TestReadLocalState_DetachedHead_ResolvesCommit(t *testing.T) {
	dir := t.TempDir()
	want := commitRealGitRepo(t, dir)
	// Rewrite HEAD to a raw commit id (detached).
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".git", "HEAD"),
		[]byte(want+"\n"), 0o600))

	st, err := ReadLocalState(dir)
	require.NoError(t, err)
	assert.Equal(t, want, st.HeadCommit)
	assert.Empty(t, st.Ref)
}
