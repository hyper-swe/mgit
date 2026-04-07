package service

import (
	"context"
	"fmt"
	"time"

	"github.com/astutic/mgit/internal/model"
	gitstore "github.com/astutic/mgit/internal/store/git"
	"github.com/astutic/mgit/internal/store/index"
)

// BranchService manages branch lifecycle operations.
// Refs: FR-5, MGIT-3.1.4
type BranchService struct {
	gitBranches *gitstore.BranchStore
	indexStore  *index.Store
	repo        *gitstore.Repository
}

// NewBranchService creates a BranchService with injected dependencies.
func NewBranchService(repo *gitstore.Repository, bs *gitstore.BranchStore, idx *index.Store) *BranchService {
	return &BranchService{
		gitBranches: bs,
		indexStore:  idx,
		repo:        repo,
	}
}

// CreateBranch creates a new task branch with auto-naming convention.
// Branch name: task/{TaskID} (e.g., task/MGIT-1.2.3).
// Refs: FR-5
func (s *BranchService) CreateBranch(ctx context.Context, taskID string) (*model.Branch, error) {
	tid, err := model.ParseTaskID(taskID)
	if err != nil {
		return nil, fmt.Errorf("create branch: %w", err)
	}

	head, err := s.repo.Head()
	if err != nil {
		return nil, fmt.Errorf("create branch: resolve HEAD: %w", err)
	}

	branchName := "task/" + taskID
	branch := &model.Branch{
		Name:       branchName,
		TaskID:     tid,
		HeadCommit: head,
		CreatedAt:  s.repo.Now(),
	}

	// Create in go-git
	if err := s.gitBranches.CreateBranch(ctx, branch); err != nil {
		return nil, fmt.Errorf("create branch in git: %w", err)
	}

	// Create in SQLite index
	if err := s.indexStore.CreateBranch(ctx, branch); err != nil {
		return nil, fmt.Errorf("create branch in index: %w", err)
	}

	return branch, nil
}

// GetBranch retrieves a branch by name.
// Refs: FR-5
func (s *BranchService) GetBranch(ctx context.Context, name string) (*model.Branch, error) {
	return s.gitBranches.GetBranch(ctx, name)
}

// ListBranches returns all branches.
// Refs: FR-5
func (s *BranchService) ListBranches(ctx context.Context) ([]*model.Branch, error) {
	return s.gitBranches.ListBranches(ctx)
}

// SwitchBranch switches HEAD to the named branch.
// Refs: FR-5
func (s *BranchService) SwitchBranch(ctx context.Context, name string) error {
	return s.gitBranches.SwitchBranch(ctx, name)
}

// DeleteBranch removes a branch. Rejects unmerged branches unless forced.
// Refs: FR-5
func (s *BranchService) DeleteBranch(ctx context.Context, name string, force bool) error {
	return s.gitBranches.DeleteBranch(ctx, name, force)
}

// LockBranch acquires an advisory lock on a branch.
// Refs: FR-5, NFR-3.5
func (s *BranchService) LockBranch(ctx context.Context, name, agentID string, duration time.Duration) error {
	return s.indexStore.LockBranch(ctx, name, agentID, duration)
}

// UnlockBranch releases the advisory lock on a branch.
// Refs: FR-5
func (s *BranchService) UnlockBranch(ctx context.Context, name string) error {
	return s.indexStore.UnlockBranch(ctx, name)
}
