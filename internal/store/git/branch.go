package git

import (
	"context"
	"fmt"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/storer"

	"github.com/hyper-swe/mgit/internal/model"
)

// BranchStore manages branch references in the go-git store.
// Branches are stored as go-git refs under refs/heads/.
// Refs: FR-5, MGIT-2.2.4
type BranchStore struct {
	repo *Repository
}

// NewBranchStore creates a BranchStore backed by the given Repository.
func NewBranchStore(repo *Repository) *BranchStore {
	return &BranchStore{repo: repo}
}

// CreateBranch creates a new branch reference pointing to the given commit.
// Returns ErrBranchAlreadyExists if a branch with that name exists.
// Refs: FR-5
func (bs *BranchStore) CreateBranch(_ context.Context, branch *model.Branch) error {
	refName := plumbing.NewBranchReferenceName(branch.Name)

	// Check if branch already exists
	_, err := bs.repo.repo.Storer.Reference(refName)
	if err == nil {
		return fmt.Errorf("%w: %s", model.ErrBranchAlreadyExists, branch.Name)
	}

	// Create the branch ref
	hash := plumbing.NewHash(branch.HeadCommit)
	ref := plumbing.NewHashReference(refName, hash)

	if err := bs.repo.repo.Storer.SetReference(ref); err != nil {
		return fmt.Errorf("create branch ref: %w", err)
	}

	return nil
}

// GetBranch retrieves a branch by name.
// Returns ErrBranchNotFound if the branch does not exist.
// Refs: FR-5
func (bs *BranchStore) GetBranch(_ context.Context, name string) (*model.Branch, error) {
	refName := plumbing.NewBranchReferenceName(name)
	ref, err := bs.repo.repo.Storer.Reference(refName)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", model.ErrBranchNotFound, name)
	}

	return &model.Branch{
		Name:       name,
		HeadCommit: ref.Hash().String(),
		CreatedAt:  bs.repo.Now(),
	}, nil
}

// ListBranches returns all branches in the repository.
// Refs: FR-5
func (bs *BranchStore) ListBranches(_ context.Context) ([]*model.Branch, error) {
	refs, err := bs.repo.repo.Storer.IterReferences()
	if err != nil {
		return nil, fmt.Errorf("list refs: %w", err)
	}

	var branches []*model.Branch
	err = refs.ForEach(func(ref *plumbing.Reference) error {
		if ref.Name().IsBranch() {
			branches = append(branches, &model.Branch{
				Name:       ref.Name().Short(),
				HeadCommit: ref.Hash().String(),
			})
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("iterate refs: %w", err)
	}

	return branches, nil
}

// SwitchBranch updates HEAD to point to the specified branch.
// Returns ErrBranchNotFound if the branch does not exist.
// Refs: FR-5
func (bs *BranchStore) SwitchBranch(_ context.Context, name string) error {
	refName := plumbing.NewBranchReferenceName(name)

	// Verify branch exists
	_, err := bs.repo.repo.Storer.Reference(refName)
	if err != nil {
		return fmt.Errorf("%w: %s", model.ErrBranchNotFound, name)
	}

	// Update HEAD symbolic ref
	headRef := plumbing.NewSymbolicReference(plumbing.HEAD, refName)
	if err := bs.repo.repo.Storer.SetReference(headRef); err != nil {
		return fmt.Errorf("switch branch: %w", err)
	}

	return nil
}

// DeleteBranch removes a branch reference.
// If force is false, deletion is rejected (branches should be merged first).
// If force is true, the branch is removed regardless.
// Refs: FR-5
func (bs *BranchStore) DeleteBranch(_ context.Context, name string, force bool) error {
	refName := plumbing.NewBranchReferenceName(name)

	// Verify branch exists
	_, err := bs.repo.repo.Storer.Reference(refName)
	if err != nil {
		return fmt.Errorf("%w: %s", model.ErrBranchNotFound, name)
	}

	if !force {
		return fmt.Errorf("branch %q is not merged; use force to delete", name)
	}

	if err := bs.repo.repo.Storer.RemoveReference(refName); err != nil {
		// Check if storer supports removal
		if remover, ok := bs.repo.repo.Storer.(storer.ReferenceStorer); ok {
			if err2 := remover.RemoveReference(refName); err2 != nil {
				return fmt.Errorf("delete branch: %w", err2)
			}
			return nil
		}
		return fmt.Errorf("delete branch: %w", err)
	}

	return nil
}
