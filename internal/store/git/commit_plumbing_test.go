package git

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCreateCommit_ViaPlumbing_NoGitIndex verifies that a commit is created
// entirely in the .mgit object store via plumbing: no `.git` directory and no
// go-git index file are ever created at the project root or under .mgit/.
// Refs: MGIT-14.3
func TestCreateCommit_ViaPlumbing_NoGitIndex(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()
	ws := NewWorktreeStore(repo)
	cs := NewCommitStore(repo)

	require.NoError(t, os.WriteFile(filepath.Join(repo.Root(), "a.go"), []byte("package a\n"), 0o600))
	require.NoError(t, ws.Add(ctx, "a.go"))

	c := makeTestModelCommit(t, "MGIT-1.1")
	hash, err := cs.CreateCommit(ctx, c)
	require.NoError(t, err)
	assert.Len(t, hash, 40)

	// No project-root `.git` was created.
	_, err = os.Stat(filepath.Join(repo.Root(), ".git"))
	assert.True(t, os.IsNotExist(err), "mgit must not create a project-root .git")

	// No go-git index inside the .mgit store.
	_, err = os.Stat(filepath.Join(repo.MgitDir(), "index"))
	assert.True(t, os.IsNotExist(err), "mgit must not create a go-git .git/index")

	// The committed tree actually contains the staged file content.
	got, err := cs.GetFileFromCommit(ctx, hash, "a.go")
	require.NoError(t, err)
	assert.Equal(t, "package a\n", string(got))
}

// TestStaging_MgitOwned_NoGogitIndex verifies the staging area lives in mgit's
// own staging file under .mgit/ (not a go-git index), and that committing
// clears it.
// Refs: MGIT-14.3
func TestStaging_MgitOwned_NoGogitIndex(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()
	ws := NewWorktreeStore(repo)
	cs := NewCommitStore(repo)

	require.NoError(t, os.WriteFile(filepath.Join(repo.Root(), "s.go"), []byte("package s\n"), 0o600))
	require.NoError(t, ws.Add(ctx, "s.go"))

	// Staging is recorded in mgit's own staging.json under .mgit/.
	assert.FileExists(t, filepath.Join(repo.MgitDir(), stagingFileName))
	staged, err := repo.stagedPaths()
	require.NoError(t, err)
	assert.Equal(t, []string{"s.go"}, staged)

	// A staged file shows dirty in status until committed.
	clean, dirty, err := ws.IsClean(ctx)
	require.NoError(t, err)
	assert.False(t, clean)
	assert.Contains(t, dirty, "s.go")

	c := makeTestModelCommit(t, "MGIT-1.2")
	_, err = cs.CreateCommit(ctx, c)
	require.NoError(t, err)

	// Commit clears the staging area; worktree is clean afterward.
	staged, err = repo.stagedPaths()
	require.NoError(t, err)
	assert.Empty(t, staged)
	clean, _, err = ws.IsClean(ctx)
	require.NoError(t, err)
	assert.True(t, clean)
}

// TestCreateCommit_TreeMatchesFileDiffs verifies the landed tree matches the
// set of staged file changes: every staged add/modify appears in the tree with
// the on-disk content, and a staged deletion is removed from the tree.
// Refs: MGIT-14.3
func TestCreateCommit_TreeMatchesFileDiffs(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()
	ws := NewWorktreeStore(repo)
	cs := NewCommitStore(repo)
	ts := NewTreeStore(repo)

	// First commit: add two files.
	require.NoError(t, os.WriteFile(filepath.Join(repo.Root(), "keep.go"), []byte("package keep\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(repo.Root(), "gone.go"), []byte("package gone\n"), 0o600))
	require.NoError(t, ws.Add(ctx, "keep.go"))
	require.NoError(t, ws.Add(ctx, "gone.go"))
	first := makeTestModelCommit(t, "MGIT-2.1")
	hash1, err := cs.CreateCommit(ctx, first)
	require.NoError(t, err)

	entries1, err := ts.TraverseTree(ctx, treeOfCommit(t, repo, hash1))
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"gone.go", "keep.go"}, pathsOf(entries1))

	// Second commit: modify keep.go and delete gone.go.
	require.NoError(t, os.WriteFile(filepath.Join(repo.Root(), "keep.go"), []byte("package keep // v2\n"), 0o600))
	require.NoError(t, os.Remove(filepath.Join(repo.Root(), "gone.go")))
	require.NoError(t, ws.Add(ctx, "keep.go"))
	require.NoError(t, ws.Add(ctx, "gone.go")) // stage the deletion
	second := makeTestModelCommit(t, "MGIT-2.2")
	hash2, err := cs.CreateCommit(ctx, second)
	require.NoError(t, err)

	entries2, err := ts.TraverseTree(ctx, treeOfCommit(t, repo, hash2))
	require.NoError(t, err)
	assert.Equal(t, []string{"keep.go"}, pathsOf(entries2), "deletion must drop gone.go from the tree")

	got, err := cs.GetFileFromCommit(ctx, hash2, "keep.go")
	require.NoError(t, err)
	assert.Equal(t, "package keep // v2\n", string(got))
}

func treeOfCommit(t *testing.T, repo *Repository, commitHash string) string {
	t.Helper()
	c, err := repo.repo.CommitObject(hashFromString(commitHash))
	require.NoError(t, err)
	return c.TreeHash.String()
}

func pathsOf(entries []TreeEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Path)
	}
	sort.Strings(out)
	return out
}
