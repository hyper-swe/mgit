package git

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// statusFor returns the FileStatus for a given path, or false if absent.
func statusFor(files []FileStatus, path string) (FileStatus, bool) {
	for _, f := range files {
		if f.Path == path {
			return f, true
		}
	}
	return FileStatus{}, false
}

func TestWorktreeStore_Add_All_StagesEveryChange(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()
	ws := NewWorktreeStore(repo)

	require.NoError(t, os.WriteFile(filepath.Join(repo.Root(), "x.go"), []byte("package x\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(repo.Root(), "y.go"), []byte("package y\n"), 0o600))

	require.NoError(t, ws.Add(ctx, "."))

	staged, err := repo.stagedPaths()
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"x.go", "y.go"}, staged)
}

func TestWorktreeStore_Add_Idempotent(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()
	ws := NewWorktreeStore(repo)

	require.NoError(t, os.WriteFile(filepath.Join(repo.Root(), "dup.go"), []byte("package dup\n"), 0o600))
	require.NoError(t, ws.Add(ctx, "dup.go"))
	require.NoError(t, ws.Add(ctx, "dup.go"))

	staged, err := repo.stagedPaths()
	require.NoError(t, err)
	assert.Equal(t, []string{"dup.go"}, staged)
}

func TestWorktreeStore_Add_RejectsEscapingPath(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()
	ws := NewWorktreeStore(repo)

	assert.Error(t, ws.Add(ctx, "../escape.go"))
	assert.Error(t, ws.Add(ctx, ".git/config"))
	assert.Error(t, ws.Add(ctx, ".mgit/index.db"))
}

func TestWorktreeStore_Status_ModifiedTrackedFile(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()
	ws := NewWorktreeStore(repo)
	cs := NewCommitStore(repo)

	require.NoError(t, os.WriteFile(filepath.Join(repo.Root(), "m.go"), []byte("v1\n"), 0o600))
	require.NoError(t, ws.Add(ctx, "m.go"))
	_, err := cs.CreateCommit(ctx, makeTestModelCommit(t, "MGIT-6.1"))
	require.NoError(t, err)

	// Modify the committed file without staging — must show worktree "M".
	require.NoError(t, os.WriteFile(filepath.Join(repo.Root(), "m.go"), []byte("v2\n"), 0o600))
	files, err := ws.Status(ctx)
	require.NoError(t, err)
	st, ok := statusFor(files, "m.go")
	require.True(t, ok)
	assert.Equal(t, "M", st.Worktree)
	assert.Equal(t, " ", st.Staging)
}

func TestWorktreeStore_Status_DeletedTrackedFile(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()
	ws := NewWorktreeStore(repo)
	cs := NewCommitStore(repo)

	require.NoError(t, os.WriteFile(filepath.Join(repo.Root(), "d.go"), []byte("v1\n"), 0o600))
	require.NoError(t, ws.Add(ctx, "d.go"))
	_, err := cs.CreateCommit(ctx, makeTestModelCommit(t, "MGIT-6.2"))
	require.NoError(t, err)

	require.NoError(t, os.Remove(filepath.Join(repo.Root(), "d.go")))
	files, err := ws.Status(ctx)
	require.NoError(t, err)
	st, ok := statusFor(files, "d.go")
	require.True(t, ok)
	assert.Equal(t, "D", st.Worktree)
}

func TestWorktreeStore_Clean_RemovesUntrackedOnly(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()
	ws := NewWorktreeStore(repo)
	cs := NewCommitStore(repo)

	// Tracked file (committed) must survive clean.
	require.NoError(t, os.WriteFile(filepath.Join(repo.Root(), "tracked.go"), []byte("t\n"), 0o600))
	require.NoError(t, ws.Add(ctx, "tracked.go"))
	_, err := cs.CreateCommit(ctx, makeTestModelCommit(t, "MGIT-6.3"))
	require.NoError(t, err)

	// Untracked file must be removed.
	require.NoError(t, os.WriteFile(filepath.Join(repo.Root(), "junk.tmp"), []byte("junk"), 0o600))

	require.NoError(t, ws.Clean(ctx))

	assert.FileExists(t, filepath.Join(repo.Root(), "tracked.go"))
	_, statErr := os.Stat(filepath.Join(repo.Root(), "junk.tmp"))
	assert.True(t, os.IsNotExist(statErr), "untracked file must be removed by clean")
}

func TestWorktreeStore_Checkout_NonexistentBranch(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()
	ws := NewWorktreeStore(repo)

	err := ws.Checkout(ctx, "no-such-branch")
	assert.ErrorIs(t, err, model.ErrBranchNotFound)
}

func TestRepository_ClearStaging_NoStagingFile(t *testing.T) {
	repo := initTestRepo(t)
	// clearStaging must be a no-op when nothing is staged.
	assert.NoError(t, repo.clearStaging())
}

func TestCreateCommit_NestedPaths_BuildsHierarchicalTree(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()
	ws := NewWorktreeStore(repo)
	cs := NewCommitStore(repo)

	require.NoError(t, os.MkdirAll(filepath.Join(repo.Root(), "a", "b"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(repo.Root(), "a", "b", "c.go"), []byte("package c\n"), 0o600))
	require.NoError(t, ws.Add(ctx, "a/b/c.go"))

	hash, err := cs.CreateCommit(ctx, makeTestModelCommit(t, "MGIT-6.4"))
	require.NoError(t, err)

	got, err := cs.GetFileFromCommit(ctx, hash, "a/b/c.go")
	require.NoError(t, err)
	assert.Equal(t, "package c\n", string(got))
}
