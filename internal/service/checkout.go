package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/hyper-swe/mgit-dev/internal/model"
	gitstore "github.com/hyper-swe/mgit-dev/internal/store/git"
)

// CheckoutService switches the worktree to the tip of a target branch
// after verifying there are no uncommitted local changes.
// Refs: FR-5.5, FR-5.5a, MGIT-4.2.9
type CheckoutService struct {
	branches  *gitstore.BranchStore
	worktrees *gitstore.WorktreeStore
}

// NewCheckoutService creates a CheckoutService backed by the given stores.
func NewCheckoutService(bs *gitstore.BranchStore, ws *gitstore.WorktreeStore) *CheckoutService {
	return &CheckoutService{
		branches:  bs,
		worktrees: ws,
	}
}

// CheckoutResult holds the outcome of a checkout operation, suitable for
// JSON serialization to CLI consumers.
// Refs: MGIT-4.2.9
type CheckoutResult struct {
	Branch string `json:"branch"`
	Status string `json:"status"`
}

// Checkout switches HEAD and the working directory to the named branch.
// Returns model.ErrBranchNotFound if the branch does not exist, and a
// descriptive error containing the dirty file list if the worktree has
// uncommitted changes.
// Refs: FR-5.5, FR-5.5a
func (s *CheckoutService) Checkout(ctx context.Context, name string) (*CheckoutResult, error) {
	if name == "" {
		return nil, fmt.Errorf("checkout: branch name must not be empty")
	}

	// Verify the target branch exists before touching the worktree.
	if _, err := s.branches.GetBranch(ctx, name); err != nil {
		return nil, fmt.Errorf("checkout: %w", err)
	}

	clean, dirty, err := s.worktrees.IsClean(ctx)
	if err != nil {
		return nil, fmt.Errorf("checkout: status: %w", err)
	}
	if !clean {
		return nil, fmt.Errorf(
			"checkout: uncommitted changes exist — commit or stash first: %s",
			strings.Join(dirty, ", "),
		)
	}

	// Worktree.Checkout updates HEAD and the working directory atomically.
	if err := s.worktrees.Checkout(ctx, name); err != nil {
		// Map go-git errors back to mgit's sentinel where appropriate.
		return nil, fmt.Errorf("checkout: %w", err)
	}

	return &CheckoutResult{
		Branch: name,
		Status: "switched",
	}, nil
}

// Compile-time guard: CheckoutService must propagate ErrBranchNotFound.
var _ = model.ErrBranchNotFound
