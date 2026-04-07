package service

import (
	"context"
	"fmt"
	"time"

	"github.com/astutic/mgit/internal/model"
	"github.com/astutic/mgit/internal/store/index"
)

// WorktreeService orchestrates worktree lifecycle with task binding.
// Wraps the SQLite worktree registry and coordinates with BranchService.
// Refs: FR-16, MGIT-8.2.1
type WorktreeService struct {
	indexStore *index.Store
	branch     *BranchService
	clock      func() time.Time
}

// NewWorktreeService creates a WorktreeService with injected dependencies.
func NewWorktreeService(idx *index.Store, branch *BranchService, clock func() time.Time) *WorktreeService {
	return &WorktreeService{
		indexStore: idx,
		branch:     branch,
		clock:      clock,
	}
}

// Add creates a new worktree bound to a task.
// Auto-creates the task branch if it doesn't exist.
// Refs: FR-16.1, FR-16.2
func (s *WorktreeService) Add(ctx context.Context, opts model.WorktreeAddOptions) (*model.WorktreeInfo, error) {
	if err := opts.Validate(); err != nil {
		return nil, fmt.Errorf("worktree add: %w", err)
	}

	// Derive branch name from task if not provided
	branchName := opts.Branch
	if branchName == "" {
		branchName = "task/" + opts.TaskID
	}

	// Auto-create branch if needed
	_, err := s.branch.GetBranch(ctx, branchName)
	if err != nil {
		_, createErr := s.branch.CreateBranch(ctx, opts.TaskID)
		if createErr != nil {
			return nil, fmt.Errorf("worktree add: create branch: %w", createErr)
		}
	}

	wt := &model.WorktreeInfo{
		Path:      opts.Path,
		Name:      model.DeriveNameFromPath(opts.Path),
		Branch:    branchName,
		TaskID:    opts.TaskID,
		AgentID:   opts.AgentID,
		CreatedAt: s.clock(),
	}

	// Register in SQLite (UNIQUE constraints enforce isolation)
	if err := s.indexStore.InsertWorktree(ctx, wt); err != nil {
		return nil, fmt.Errorf("worktree add: %w", err)
	}

	return wt, nil
}

// List returns all registered worktrees.
// Refs: FR-16
func (s *WorktreeService) List(ctx context.Context) ([]model.WorktreeInfo, error) {
	return s.indexStore.ListWorktrees(ctx)
}

// Remove deletes a worktree registration.
// Refs: FR-16
func (s *WorktreeService) Remove(ctx context.Context, path string, _ bool) error {
	return s.indexStore.DeleteWorktree(ctx, path)
}

// Resolve looks up a worktree by path.
// Refs: FR-16
func (s *WorktreeService) Resolve(ctx context.Context, path string) (*model.WorktreeInfo, error) {
	return s.indexStore.GetWorktree(ctx, path)
}
