// Package e2e — checkout CLI integration tests.
// Refs: FR-5.5, FR-5.5a, MGIT-4.2.9
package e2e

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit-dev/internal/model"
)

// TestCheckout_ValidBranch_SwitchesWorkingDir creates a task branch then
// switches to it and back to main, asserting that HEAD follows along.
// Refs: FR-5.5, MGIT-4.2.9
func TestCheckout_ValidBranch_SwitchesWorkingDir(t *testing.T) {
	env := setupServiceEnv(t)
	ctx := context.Background()

	// Create a task branch off of main.
	branch, err := env.branch.CreateBranch(ctx, "MGIT-4.2.9")
	require.NoError(t, err)
	require.Equal(t, "task/MGIT-4.2.9", branch.Name)

	result, err := env.checkout.Checkout(ctx, "task/MGIT-4.2.9")
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, "task/MGIT-4.2.9", result.Branch)
	assert.Equal(t, "switched", result.Status)

	// Switch back to main.
	result, err = env.checkout.Checkout(ctx, "main")
	require.NoError(t, err)
	assert.Equal(t, "main", result.Branch)
}

// TestCheckout_UncommittedChanges_ReturnsError verifies that a dirty
// worktree blocks the checkout with a descriptive error.
// Refs: FR-5.5a, MGIT-4.2.9
func TestCheckout_UncommittedChanges_ReturnsError(t *testing.T) {
	env := setupServiceEnv(t)
	ctx := context.Background()

	_, err := env.branch.CreateBranch(ctx, "MGIT-4.2.9")
	require.NoError(t, err)

	// Introduce an uncommitted change in the working directory.
	dirtyFile := filepath.Join(env.repo.Root(), "dirty.txt")
	require.NoError(t, os.WriteFile(dirtyFile, []byte("uncommitted\n"), 0o600))
	require.NoError(t, env.worktree.Add(ctx, "dirty.txt"))

	clean, dirty, err := env.worktree.IsClean(ctx)
	require.NoError(t, err)
	require.False(t, clean, "worktree should be dirty after staging a new file")
	require.NotEmpty(t, dirty)

	_, err = env.checkout.Checkout(ctx, "task/MGIT-4.2.9")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "uncommitted changes exist",
		"checkout must explain why it refused to switch branches")
}

// TestCheckout_NonExistentBranch_ReturnsError verifies that a missing
// branch returns model.ErrBranchNotFound.
// Refs: MGIT-4.2.9
func TestCheckout_NonExistentBranch_ReturnsError(t *testing.T) {
	env := setupServiceEnv(t)
	ctx := context.Background()

	_, err := env.checkout.Checkout(ctx, "task/does-not-exist")
	require.Error(t, err)
	assert.True(t, errors.Is(err, model.ErrBranchNotFound),
		"missing branch must return ErrBranchNotFound, got %v", err)

	// Empty branch name is rejected with a usage error.
	_, err = env.checkout.Checkout(ctx, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "branch name must not be empty")
}

// TestCheckout_MainBranch_Success verifies the canonical "switch back to
// main" path works from a freshly initialized repo.
// Refs: MGIT-4.2.9
func TestCheckout_MainBranch_Success(t *testing.T) {
	env := setupServiceEnv(t)
	ctx := context.Background()

	result, err := env.checkout.Checkout(ctx, "main")
	require.NoError(t, err)
	assert.Equal(t, "main", result.Branch)
	assert.Equal(t, "switched", result.Status)
}
