package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gitstore "github.com/hyper-swe/mgit/internal/store/git"
)

// --- RestoreService Tests ---
// Refs: FR-6.7, MGIT-4.2.8

// stageFile writes a file to the worktree and stages it via go-git.
func stageFile(t *testing.T, env *testEnv, relPath, content string) {
	t.Helper()
	repoRoot := filepath.Dir(env.repo.MgitDir())
	absPath := filepath.Join(repoRoot, relPath)
	require.NoError(t, os.MkdirAll(filepath.Dir(absPath), 0o750))
	require.NoError(t, os.WriteFile(absPath, []byte(content), 0o600))
	ws := gitstore.NewWorktreeStore(env.repo)
	require.NoError(t, ws.Add(context.Background(), relPath))
}

func TestRestoreService_RestoreFile_Valid(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	repoRoot := filepath.Dir(env.repo.MgitDir())

	// Write and stage a file so the commit tree contains it.
	stageFile(t, env, "hello.txt", "hello world")

	c, err := env.commit.CreateCommit(ctx, CreateCommitRequest{
		TaskID: "MGIT-13.1", AgentID: "a", Message: "add hello",
	})
	require.NoError(t, err)

	// Remove the file so we can restore it.
	require.NoError(t, os.Remove(filepath.Join(repoRoot, "hello.txt")))

	restoreSvc := NewRestoreService(env.repo, env.cs, repoRoot)
	result, err := restoreSvc.RestoreFile(ctx, "hello.txt", c.CommitID)
	require.NoError(t, err)
	assert.Equal(t, "hello.txt", result.Path)
	assert.Equal(t, "restored", result.Status)

	// Verify the file was actually restored.
	contents, err := os.ReadFile(filepath.Join(repoRoot, "hello.txt")) //nolint:gosec // test reads a known file from a temp directory
	require.NoError(t, err)
	assert.Equal(t, "hello world", string(contents))
}

func TestRestoreService_RestoreFile_EmptyPath(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()
	repoRoot := filepath.Dir(env.repo.MgitDir())

	restoreSvc := NewRestoreService(env.repo, env.cs, repoRoot)
	_, err := restoreSvc.RestoreFile(ctx, "", "somehash")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "path must not be empty")
}

func TestRestoreService_RestoreFile_EmptyHash(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()
	repoRoot := filepath.Dir(env.repo.MgitDir())

	restoreSvc := NewRestoreService(env.repo, env.cs, repoRoot)
	_, err := restoreSvc.RestoreFile(ctx, "main.go", "")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "commit hash must not be empty")
}

func TestRestoreService_RestoreFile_PathTraversal(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()
	repoRoot := filepath.Dir(env.repo.MgitDir())

	restoreSvc := NewRestoreService(env.repo, env.cs, repoRoot)

	tests := []struct {
		name string
		path string
	}{
		{"dotdot", "../etc/passwd"},
		{"absolute", "/etc/passwd"},
		{"dotdot_nested", "foo/../../etc/passwd"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := restoreSvc.RestoreFile(ctx, tt.path, "abc123")
			assert.Error(t, err)
			assert.Contains(t, err.Error(), "refusing path")
		})
	}
}

func TestRestoreService_RestoreFile_InvalidHash(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()
	repoRoot := filepath.Dir(env.repo.MgitDir())

	restoreSvc := NewRestoreService(env.repo, env.cs, repoRoot)
	_, err := restoreSvc.RestoreFile(ctx, "main.go", "nonexistenthash")
	assert.Error(t, err)
}

func TestRestoreService_RestoreFile_CreatesParentDir(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	repoRoot := filepath.Dir(env.repo.MgitDir())

	// Write and stage a nested file.
	stageFile(t, env, "sub/dir/nested.txt", "nested content")

	c, err := env.commit.CreateCommit(ctx, CreateCommitRequest{
		TaskID: "MGIT-13.2", AgentID: "a", Message: "add nested",
	})
	require.NoError(t, err)

	// Remove the parent dir.
	require.NoError(t, os.RemoveAll(filepath.Join(repoRoot, "sub")))

	restoreSvc := NewRestoreService(env.repo, env.cs, repoRoot)
	result, err := restoreSvc.RestoreFile(ctx, "sub/dir/nested.txt", c.CommitID)
	require.NoError(t, err)
	assert.Equal(t, "restored", result.Status)
	assert.Greater(t, result.BytesWrit, 0)
}
