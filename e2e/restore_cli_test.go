// Package e2e — restore CLI integration tests.
// Refs: FR-6.7, MGIT-4.2.8
package e2e

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/service"
	gitstore "github.com/hyper-swe/mgit/internal/store/git"
)

// stageAndCommit writes `content` to `relPath` inside the env's worktree,
// stages it, and creates an mgit commit. Returns the resulting commit hash.
func stageAndCommit(t *testing.T, env *serviceEnv, relPath, content, taskID string) string {
	t.Helper()
	ctx := context.Background()

	target := filepath.Join(env.repo.Root(), relPath)
	require.NoError(t, os.MkdirAll(filepath.Dir(target), 0o750))
	require.NoError(t, os.WriteFile(target, []byte(content), 0o600))

	ws := gitstore.NewWorktreeStore(env.repo)
	require.NoError(t, ws.Add(ctx, relPath))

	c, err := env.commit.CreateCommit(ctx, service.CreateCommitRequest{
		TaskID:  taskID,
		AgentID: "restore-test",
		Message: "stage " + relPath,
	})
	require.NoError(t, err)
	return c.CommitID
}

// TestRestore_ValidFileAndCommit_RestoresContent verifies the canonical
// happy path: a file modified after a commit is restored to its original
// content from that commit.
// Refs: FR-6.7, MGIT-4.2.8
func TestRestore_ValidFileAndCommit_RestoresContent(t *testing.T) {
	env := setupServiceEnv(t)
	ctx := context.Background()

	const original = "original line\n"
	const modified = "modified line\n"
	const relPath = "src/hello.txt"

	hash := stageAndCommit(t, env, relPath, original, "MGIT-4.2.8")

	// Tamper with the file in the working directory.
	target := filepath.Join(env.repo.Root(), relPath)
	require.NoError(t, os.WriteFile(target, []byte(modified), 0o600))

	rs := service.NewRestoreService(gitstore.NewCommitStore(env.repo), env.repo.Root())
	result, err := rs.RestoreFile(ctx, relPath, hash)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, relPath, result.Path)
	assert.Equal(t, hash, result.CommitHash)
	assert.Equal(t, len(original), result.BytesWrit)
	assert.Equal(t, "restored", result.Status)

	got, err := os.ReadFile(target) //nolint:gosec // test path
	require.NoError(t, err)
	assert.Equal(t, original, string(got),
		"working-directory file must match the committed content after restore")
}

// TestRestore_InvalidCommit_ReturnsError verifies that a missing commit
// hash propagates ErrCommitNotFound to callers.
// Refs: MGIT-4.2.8
func TestRestore_InvalidCommit_ReturnsError(t *testing.T) {
	env := setupServiceEnv(t)
	ctx := context.Background()

	// Seed at least one valid commit so the worktree is non-empty.
	stageAndCommit(t, env, "a.txt", "hello\n", "MGIT-4.2.8")

	rs := service.NewRestoreService(gitstore.NewCommitStore(env.repo), env.repo.Root())
	_, err := rs.RestoreFile(ctx, "a.txt", "0000000000000000000000000000000000000000")
	require.Error(t, err)
	assert.True(t, errors.Is(err, model.ErrCommitNotFound),
		"unknown commit hash must return ErrCommitNotFound, got %v", err)
}

// TestRestore_FileNotInCommit_ReturnsError verifies that a path absent
// from the commit's tree returns ErrFileNotFound.
// Refs: MGIT-4.2.8
func TestRestore_FileNotInCommit_ReturnsError(t *testing.T) {
	env := setupServiceEnv(t)
	ctx := context.Background()

	hash := stageAndCommit(t, env, "exists.txt", "yes\n", "MGIT-4.2.8")

	rs := service.NewRestoreService(gitstore.NewCommitStore(env.repo), env.repo.Root())
	_, err := rs.RestoreFile(ctx, "missing.txt", hash)
	require.Error(t, err)
	assert.True(t, errors.Is(err, model.ErrFileNotFound),
		"missing file must return ErrFileNotFound, got %v", err)
}

// TestRestore_NoArgs_ReturnsUsageError verifies the service rejects an
// empty path or empty commit hash with a clear usage error. The CLI layer
// (cobra ExactArgs(1)) handles the missing-positional case at parse time.
// Refs: MGIT-4.2.8
func TestRestore_NoArgs_ReturnsUsageError(t *testing.T) {
	env := setupServiceEnv(t)
	ctx := context.Background()

	rs := service.NewRestoreService(gitstore.NewCommitStore(env.repo), env.repo.Root())

	_, err := rs.RestoreFile(ctx, "", "deadbeef")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "path must not be empty")

	_, err = rs.RestoreFile(ctx, "a.txt", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "commit hash must not be empty")

	// Path traversal must be rejected.
	_, err = rs.RestoreFile(ctx, "../escape.txt", "deadbeef")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "refusing path")
}
