package service

import (
	"context"
	"errors"
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

	// Create in SQLite index (self-heals a stale orphan; rolls back the ref on
	// an unrecoverable index failure — MGIT-42).
	if err := s.insertBranchIndex(ctx, branch); err != nil {
		return nil, err
	}

	return branch, nil
}

// insertBranchIndex inserts branch into the SQLite index, keeping the two stores
// consistent when the index write trips. The caller has ALREADY created the git
// ref above, and gitBranches.CreateBranch rejects a genuine duplicate at the ref
// step — so an ErrBranchAlreadyExists here can only be a STALE index row left by
// a prior ref-only delete (MGIT-42); we clear it and re-insert (self-heal). If
// the index write fails for any other reason, or healing fails, we roll back the
// ref we just created so a failed create never leaves a partial branch.
// Refs: MGIT-42, FR-5
func (s *BranchService) insertBranchIndex(ctx context.Context, branch *model.Branch) error {
	err := s.indexStore.CreateBranch(ctx, branch)
	if err == nil {
		return nil
	}
	if errors.Is(err, model.ErrBranchAlreadyExists) {
		if delErr := s.indexStore.DeleteBranch(ctx, branch.Name); delErr == nil {
			if err2 := s.indexStore.CreateBranch(ctx, branch); err2 == nil {
				return nil
			}
		}
	}
	// Unrecoverable: undo the git ref so both stores stay consistent (atomicity).
	_ = s.gitBranches.DeleteBranch(ctx, branch.Name, true)
	return fmt.Errorf("create branch in index: %w", err)
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
	if err := s.insertBranchIndex(ctx, branch); err != nil {
		return nil, err
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

// DeleteBranch removes a branch from BOTH the go-git ref store and the SQLite
// index, in one operation. Deleting only the ref (the old behavior) orphaned the
// index row, so `worktree add` for the same task later failed "branch already
// exists" with no clean recovery (MGIT-42). A ref that is already absent must
// NOT block clearing the index row — that is the recovery path for an
// already-stranded task — so ErrBranchNotFound from the ref store is tolerated;
// any other ref error (e.g. an unmerged branch without --force) is real and
// propagates without touching the index. Refs: MGIT-42, FR-5
func (s *BranchService) DeleteBranch(ctx context.Context, name string, force bool) error {
	if err := s.gitBranches.DeleteBranch(ctx, name, force); err != nil && !errors.Is(err, model.ErrBranchNotFound) {
		return err
	}
	if err := s.indexStore.DeleteBranch(ctx, name); err != nil {
		return fmt.Errorf("delete branch index: %w", err)
	}
	return nil
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
