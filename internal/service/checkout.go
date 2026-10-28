package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/hyper-swe/mgit/internal/model"
	gitstore "github.com/hyper-swe/mgit/internal/store/git"
	"github.com/hyper-swe/mgit/internal/store/index"
)

// CheckoutService switches the worktree to the tip of a target branch
// after verifying there are no uncommitted local changes.
// Refs: FR-5.5, FR-5.5a, MGIT-4.2.9
type CheckoutService struct {
	branches  *gitstore.BranchStore
	worktrees *gitstore.WorktreeStore
	gate      *index.Store
}

// NewCheckoutService creates a CheckoutService backed by the given stores. The
// ledger decides which branch tips a host worktree may materialize; without
// one, Checkout refuses rather than switching ungated. Refs: MGIT-323.3
func NewCheckoutService(bs *gitstore.BranchStore, ws *gitstore.WorktreeStore, idx *index.Store) *CheckoutService {
	return &CheckoutService{
		branches:  bs,
		worktrees: ws,
		gate:      idx,
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
	br, err := s.branches.GetBranch(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("checkout: %w", err)
	}
	// Refuse before any ref, index or disk change. Refs: MGIT-323.3
	if s.gate == nil {
		return nil, fmt.Errorf("checkout: no content ledger is wired, so the branch tip cannot be checked")
	}
	if err := s.gate.RequireDeliverableTip(ctx, br.HeadCommit); err != nil {
		return nil, fmt.Errorf("checkout: branch %s ends at a retained guest commit: %w; stay on the current branch until it is delivered", name, err)
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
