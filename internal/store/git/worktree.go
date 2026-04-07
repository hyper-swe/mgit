package git

import (
	"context"
	"fmt"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"

	"github.com/astutic/mgit/internal/model"
)

// FileStatus represents the status of a file in the worktree.
// Refs: FR-8 (status command), MGIT-2.2.6
type FileStatus struct {
	Path     string `json:"path"`
	Staging  string `json:"staging"`
	Worktree string `json:"worktree"`
}

// WorktreeStore provides worktree operations (status, add, checkout).
// Refs: FR-2, FR-8, MGIT-2.2.6
type WorktreeStore struct {
	repo *Repository
}

// NewWorktreeStore creates a WorktreeStore backed by the given Repository.
func NewWorktreeStore(repo *Repository) *WorktreeStore {
	return &WorktreeStore{repo: repo}
}

// Status returns the status of all files in the worktree.
// Refs: FR-8
func (ws *WorktreeStore) Status(_ context.Context) ([]FileStatus, error) {
	wt, err := ws.repo.repo.Worktree()
	if err != nil {
		return nil, fmt.Errorf("get worktree: %w", err)
	}

	status, err := wt.Status()
	if err != nil {
		return nil, fmt.Errorf("worktree status: %w", err)
	}

	var files []FileStatus
	for path, s := range status {
		files = append(files, FileStatus{
			Path:     path,
			Staging:  string(s.Staging),
			Worktree: string(s.Worktree),
		})
	}

	return files, nil
}

// Add stages a file for the next commit.
// Refs: FR-2
func (ws *WorktreeStore) Add(_ context.Context, path string) error {
	wt, err := ws.repo.repo.Worktree()
	if err != nil {
		return fmt.Errorf("get worktree: %w", err)
	}

	_, err = wt.Add(path)
	if err != nil {
		return fmt.Errorf("add %s: %w", path, err)
	}

	return nil
}

// Checkout switches the worktree to the specified branch.
// Returns ErrBranchNotFound if the branch does not exist.
// Refs: FR-5, FR-8
func (ws *WorktreeStore) Checkout(_ context.Context, branchName string) error {
	wt, err := ws.repo.repo.Worktree()
	if err != nil {
		return fmt.Errorf("get worktree: %w", err)
	}

	err = wt.Checkout(&gogit.CheckoutOptions{
		Branch: plumbing.NewBranchReferenceName(branchName),
		Create: false,
	})
	if err != nil {
		return fmt.Errorf("%w: %s", model.ErrBranchNotFound, branchName)
	}

	return nil
}

// Clean removes untracked files from the worktree.
// Refs: FR-8
func (ws *WorktreeStore) Clean(_ context.Context) error {
	wt, err := ws.repo.repo.Worktree()
	if err != nil {
		return fmt.Errorf("get worktree: %w", err)
	}

	err = wt.Clean(&gogit.CleanOptions{Dir: true})
	if err != nil {
		return fmt.Errorf("clean worktree: %w", err)
	}

	return nil
}
