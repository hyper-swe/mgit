package git

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWorktreeStore_IsClean_CleanWorktree(t *testing.T) {
	repo := initTestRepo(t)
	ws := NewWorktreeStore(repo)
	ctx := context.Background()

	clean, dirty, err := ws.IsClean(ctx)
	require.NoError(t, err)
	assert.True(t, clean, "freshly initialized worktree must be clean")
	assert.Empty(t, dirty, "no dirty paths expected")
}

func TestWorktreeStore_IsClean_DirtyWorktree(t *testing.T) {
	repo := initTestRepo(t)
	ws := NewWorktreeStore(repo)
	ctx := context.Background()

	// Create an untracked file in the worktree
	err := os.WriteFile(filepath.Join(repo.Root(), "dirty.go"), []byte("package dirty\n"), 0o600)
	require.NoError(t, err)

	clean, dirty, err := ws.IsClean(ctx)
	require.NoError(t, err)
	assert.False(t, clean, "worktree with untracked file must be dirty")
	assert.Contains(t, dirty, "dirty.go")
}

func TestWorktreeStore_IsClean_MgitFilesIgnored(t *testing.T) {
	repo := initTestRepo(t)
	ws := NewWorktreeStore(repo)
	ctx := context.Background()

	// Files under .mgit/ should not cause the worktree to be "dirty"
	// The .mgit directory already exists and has internal files.
	// Verify that IsClean ignores them.
	clean, dirty, err := ws.IsClean(ctx)
	require.NoError(t, err)
	assert.True(t, clean, "mgit internal files must not count as dirty")
	for _, p := range dirty {
		assert.False(t, filepath.HasPrefix(p, ".mgit/"), "dirty list must not include .mgit/ paths")
	}
}

func TestWorktreeStore_IsClean_StagedFile(t *testing.T) {
	repo := initTestRepo(t)
	ws := NewWorktreeStore(repo)
	ctx := context.Background()

	// Create and stage a file (staged but not committed = dirty)
	err := os.WriteFile(filepath.Join(repo.Root(), "staged.go"), []byte("package staged\n"), 0o600)
	require.NoError(t, err)
	require.NoError(t, ws.Add(ctx, "staged.go"))

	clean, dirty, err := ws.IsClean(ctx)
	require.NoError(t, err)
	assert.False(t, clean, "worktree with staged file must be dirty")
	assert.Contains(t, dirty, "staged.go")
}

func TestWorktreeStore_IsClean_AfterCommit(t *testing.T) {
	repo := initTestRepo(t)
	ws := NewWorktreeStore(repo)
	ctx := context.Background()

	// Create, stage, and commit a file
	err := os.WriteFile(filepath.Join(repo.Root(), "committed.go"), []byte("package c\n"), 0o600)
	require.NoError(t, err)
	require.NoError(t, ws.Add(ctx, "committed.go"))

	cs := NewCommitStore(repo)
	c := makeTestModelCommit(t, "MGIT-1.1")
	_, err = cs.CreateCommit(ctx, c)
	require.NoError(t, err)

	clean, dirty, err := ws.IsClean(ctx)
	require.NoError(t, err)
	assert.True(t, clean, "worktree must be clean after committing all changes")
	assert.Empty(t, dirty)
}
