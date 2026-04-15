package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gitstore "github.com/hyper-swe/mgit/internal/store/git"
)

// --- CheckoutService Tests ---
// Refs: FR-5.5, FR-5.5a, MGIT-4.2.9

func setupCheckoutEnv(t *testing.T) (*testEnv, *CheckoutService) {
	t.Helper()
	env := setupTestEnv(t)
	ws := gitstore.NewWorktreeStore(env.repo)
	checkoutSvc := NewCheckoutService(env.bs, ws)
	return env, checkoutSvc
}

func TestCheckoutService_Checkout_ValidBranch(t *testing.T) {
	env, checkoutSvc := setupCheckoutEnv(t)
	ctx := context.Background()

	// Create a branch to checkout to.
	_, err := env.branch.CreateBranch(ctx, "MGIT-14.1")
	require.NoError(t, err)

	result, err := checkoutSvc.Checkout(ctx, "task/MGIT-14.1")
	require.NoError(t, err)
	assert.Equal(t, "task/MGIT-14.1", result.Branch)
	assert.Equal(t, "switched", result.Status)
}

func TestCheckoutService_Checkout_EmptyName(t *testing.T) {
	_, checkoutSvc := setupCheckoutEnv(t)
	ctx := context.Background()

	_, err := checkoutSvc.Checkout(ctx, "")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "branch name must not be empty")
}

func TestCheckoutService_Checkout_NonexistentBranch(t *testing.T) {
	_, checkoutSvc := setupCheckoutEnv(t)
	ctx := context.Background()

	_, err := checkoutSvc.Checkout(ctx, "nonexistent-branch")
	assert.Error(t, err)
}

func TestCheckoutService_Checkout_SwitchBackToMain(t *testing.T) {
	env, checkoutSvc := setupCheckoutEnv(t)
	ctx := context.Background()

	// Create a branch, checkout to it, then back to main.
	_, err := env.branch.CreateBranch(ctx, "MGIT-14.2")
	require.NoError(t, err)

	result, err := checkoutSvc.Checkout(ctx, "task/MGIT-14.2")
	require.NoError(t, err)
	assert.Equal(t, "switched", result.Status)

	result, err = checkoutSvc.Checkout(ctx, "main")
	require.NoError(t, err)
	assert.Equal(t, "main", result.Branch)
	assert.Equal(t, "switched", result.Status)
}
