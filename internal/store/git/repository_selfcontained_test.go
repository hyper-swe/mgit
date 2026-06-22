package git

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// makeProjectGitRepo creates a real git project with one commit at dir and
// returns the project HEAD hash. It is the precondition for the coexistence
// tests: mgit must never touch this .git. Refs: MGIT-14, ADR-001 amendment.
func makeProjectGitRepo(t *testing.T, dir string, clk func() time.Time) plumbing.Hash {
	t.Helper()
	projGit, err := gogit.PlainInit(dir, false)
	require.NoError(t, err)
	wt, err := projGit.Worktree()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o600))
	_, err = wt.Add("main.go")
	require.NoError(t, err)
	sig := &object.Signature{Name: "dev", Email: "dev@x", When: clk()}
	head, err := wt.Commit("real project history", &gogit.CommitOptions{Author: sig, Committer: sig})
	require.NoError(t, err)
	return head
}

// snapshotProjectGit reads the project's .git HEAD file and the resolved HEAD
// hash so a test can assert byte-for-byte stability across mgit operations.
func snapshotProjectGit(t *testing.T, dir string) (headFile []byte, headHash plumbing.Hash) {
	t.Helper()
	hf, err := os.ReadFile(filepath.Join(dir, ".git", "HEAD")) //nolint:gosec // test path
	require.NoError(t, err)
	g, err := gogit.PlainOpen(dir)
	require.NoError(t, err)
	h, err := g.Head()
	require.NoError(t, err)
	return hf, h.Hash()
}

func TestInit_OverExistingGitRepo_NoRootGitWritten(t *testing.T) {
	dir := t.TempDir()
	clk := fixedClock()
	want := makeProjectGitRepo(t, dir, clk)

	repo, err := Init(dir, clk)
	require.NoError(t, err, "mgit must initialize over an existing git project")
	t.Cleanup(func() { _ = repo.Close() })

	// The project .git must remain a real directory — mgit must never write a
	// `.git -> .mgit` gitfile at the project root.
	info, err := os.Stat(filepath.Join(dir, ".git"))
	require.NoError(t, err)
	assert.True(t, info.IsDir(), "project .git must remain a directory, not a gitfile")

	// And the project HEAD is untouched.
	g, err := gogit.PlainOpen(dir)
	require.NoError(t, err)
	h, err := g.Head()
	require.NoError(t, err)
	assert.Equal(t, want, h.Hash())
}

func TestInit_OverExistingGitRepo_ProjectGitUntouched(t *testing.T) {
	dir := t.TempDir()
	clk := fixedClock()
	_ = makeProjectGitRepo(t, dir, clk)
	beforeHead, beforeHash := snapshotProjectGit(t, dir)

	repo, err := Init(dir, clk)
	require.NoError(t, err)
	t.Cleanup(func() { _ = repo.Close() })

	// A full init + initial commit must leave the project .git HEAD byte-for-byte.
	afterHead, afterHash := snapshotProjectGit(t, dir)
	assert.Equal(t, beforeHead, afterHead, "project .git/HEAD file must be byte-for-byte unchanged")
	assert.Equal(t, beforeHash, afterHash, "project HEAD commit must be unchanged")
}

func TestInit_InitialCommitViaPlumbing_HEADValid(t *testing.T) {
	repo := initTestRepo(t)

	// HEAD must resolve to a real commit object built via plumbing.
	headHex, err := repo.Head()
	require.NoError(t, err)
	assert.Len(t, headHex, 40)

	commit, err := repo.repo.CommitObject(plumbing.NewHash(headHex))
	require.NoError(t, err, "initial commit object must exist in .mgit store")
	assert.Equal(t, "mgit", commit.Author.Name)
	assert.True(t, repo.Now().Equal(commit.Author.When), "initial commit timestamp from injected clock")

	// The initial commit has an empty tree and no parents.
	assert.Equal(t, 0, commit.NumParents(), "initial commit must have no parents")
	tree, err := commit.Tree()
	require.NoError(t, err)
	assert.Empty(t, tree.Entries, "initial commit tree must be empty")

	// HEAD points at the main branch.
	branch, err := repo.CurrentBranch()
	require.NoError(t, err)
	assert.Equal(t, "main", branch)
}

func TestOpen_SelfContainedStore(t *testing.T) {
	dir := t.TempDir()
	clk := fixedClock()
	_ = makeProjectGitRepo(t, dir, clk)

	r1, err := Init(dir, clk)
	require.NoError(t, err)
	initHead, err := r1.Head()
	require.NoError(t, err)
	_ = r1.Close()

	// Open must reopen the self-contained .mgit store (worktree-less) and see
	// the same HEAD, without requiring or writing a project worktree gitfile.
	r2, err := Open(dir, clk)
	require.NoError(t, err, "Open must succeed on a self-contained .mgit store")
	t.Cleanup(func() { _ = r2.Close() })

	openHead, err := r2.Head()
	require.NoError(t, err)
	assert.Equal(t, initHead, openHead, "reopened store must see the same HEAD")

	// Project .git still a directory and intact.
	info, err := os.Stat(filepath.Join(dir, ".git"))
	require.NoError(t, err)
	assert.True(t, info.IsDir())
}
