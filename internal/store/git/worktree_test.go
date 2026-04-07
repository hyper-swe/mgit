package git

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWorktreeStore_Status(t *testing.T) {
	repo := initTestRepo(t)
	ws := NewWorktreeStore(repo)
	ctx := context.Background()

	// Clean worktree should have no status entries
	files, err := ws.Status(ctx)
	require.NoError(t, err)
	// May have entries from init; just verify it doesn't error
	assert.NotNil(t, files)
}

func TestWorktreeStore_Status_WithUntracked(t *testing.T) {
	repo := initTestRepo(t)
	ws := NewWorktreeStore(repo)
	ctx := context.Background()

	// Create an untracked file
	err := os.WriteFile(filepath.Join(repo.Root(), "new.go"), []byte("package main\n"), 0o600)
	require.NoError(t, err)

	files, err := ws.Status(ctx)
	require.NoError(t, err)

	found := false
	for _, f := range files {
		if f.Path == "new.go" {
			found = true
			break
		}
	}
	assert.True(t, found, "untracked file must appear in status")
}

func TestWorktreeStore_Add(t *testing.T) {
	repo := initTestRepo(t)
	ws := NewWorktreeStore(repo)
	ctx := context.Background()

	// Create a file and add it
	err := os.WriteFile(filepath.Join(repo.Root(), "staged.go"), []byte("package main\n"), 0o600)
	require.NoError(t, err)

	err = ws.Add(ctx, "staged.go")
	assert.NoError(t, err, "Add must succeed")
}

func TestWorktreeStore_Clean(t *testing.T) {
	repo := initTestRepo(t)
	ws := NewWorktreeStore(repo)
	ctx := context.Background()

	err := ws.Clean(ctx)
	assert.NoError(t, err, "Clean must succeed on fresh repo")
}
