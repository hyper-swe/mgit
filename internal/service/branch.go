package service

import (
	"context"
	"fmt"
	"time"

	"github.com/hyper-swe/mgit/internal/model"
	gitstore "github.com/hyper-swe/mgit/internal/store/git"
	"github.com/hyper-swe/mgit/internal/store/index"
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

	branchName := model.TaskBranchName(taskID)
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

// CreateBranchAt creates a task branch forked at an EXPLICIT base commit rather
// than the current HEAD. It backs `mgit work --base <ref>` and the pinned
// fork-base of ADR-008 §4: the task branch is anchored at baseCommit so a later
// base resync (which advances the shared base branch) never retargets it.
// Refs: MGIT-35, FR-5
func (s *BranchService) CreateBranchAt(ctx context.Context, taskID, baseCommit string) (*model.Branch, error) {
	tid, err := model.ParseTaskID(taskID)
	if err != nil {
		return nil, fmt.Errorf("create branch: %w", err)
	}
	branch := &model.Branch{
		Name:       model.TaskBranchName(taskID),
		TaskID:     tid,
		HeadCommit: baseCommit,
		CreatedAt:  s.repo.Now(),
	}
	if err := s.gitBranches.CreateBranch(ctx, branch); err != nil {
		return nil, fmt.Errorf("create branch in git: %w", err)
	}
	if err := s.indexStore.CreateBranch(ctx, branch); err != nil {
		return nil, fmt.Errorf("create branch in index: %w", err)
	}
	return branch, nil
}

// CreateNamedBranch creates a branch with an explicit name pointing at the
// current HEAD, without the task/ auto-naming convention. It backs the
// git-familiar `mgit checkout -b <name>` create-and-switch idiom, where the
// caller supplies a literal branch name rather than a task ID.
//
// Unlike CreateBranch, the branch is written only to go-git and is
// intentionally NOT recorded in the SQLite index: a named branch carries no
// task mapping, so there is no task_commits entry to create. ListBranches and
// GetBranch read from go-git, so the branch is fully usable.
// Refs: FR-5, MGIT-23
func (s *BranchService) CreateNamedBranch(ctx context.Context, name string) (*model.Branch, error) {
	if name == "" {
		return nil, fmt.Errorf("create branch: name must not be empty")
	}

	head, err := s.repo.Head()
	if err != nil {
		return nil, fmt.Errorf("create branch: resolve HEAD: %w", err)
	}

	branch := &model.Branch{
		Name:       name,
		HeadCommit: head,
		CreatedAt:  s.repo.Now(),
	}
	if err := s.gitBranches.CreateBranch(ctx, branch); err != nil {
		return nil, fmt.Errorf("create branch in git: %w", err)
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
