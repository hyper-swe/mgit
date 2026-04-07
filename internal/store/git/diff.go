package git

import (
	"context"
	"fmt"

	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/astutic/mgit/internal/model"
)

// DiffStore computes file diffs between commits.
// Refs: FR-11, MGIT-2.2.5
type DiffStore struct {
	repo *Repository
}

// NewDiffStore creates a DiffStore backed by the given Repository.
func NewDiffStore(repo *Repository) *DiffStore {
	return &DiffStore{repo: repo}
}

// DiffCommits computes the file-level diff between two commits.
// Returns a list of FileDiffs describing what changed.
// Refs: FR-11
func (ds *DiffStore) DiffCommits(_ context.Context, fromHash, toHash string) ([]model.FileDiff, error) {
	goRepo := ds.repo.repo

	fromCommit, err := goRepo.CommitObject(hashFromString(fromHash))
	if err != nil {
		return nil, fmt.Errorf("%w: from commit %s", model.ErrCommitNotFound, fromHash)
	}
	toCommit, err := goRepo.CommitObject(hashFromString(toHash))
	if err != nil {
		return nil, fmt.Errorf("%w: to commit %s", model.ErrCommitNotFound, toHash)
	}

	fromTree, err := fromCommit.Tree()
	if err != nil {
		return nil, fmt.Errorf("get from tree: %w", err)
	}
	toTree, err := toCommit.Tree()
	if err != nil {
		return nil, fmt.Errorf("get to tree: %w", err)
	}

	changes, err := fromTree.Diff(toTree)
	if err != nil {
		return nil, fmt.Errorf("compute diff: %w", err)
	}

	diffs := make([]model.FileDiff, 0, len(changes))
	for _, change := range changes {
		fd := changeToFileDiff(change)
		diffs = append(diffs, fd)
	}

	return diffs, nil
}

// DiffStats returns aggregate statistics for a diff between two commits.
// Refs: FR-11
func (ds *DiffStore) DiffStats(ctx context.Context, fromHash, toHash string) (model.DiffStatistics, error) {
	diffs, err := ds.DiffCommits(ctx, fromHash, toHash)
	if err != nil {
		return model.DiffStatistics{}, err
	}

	var stats model.DiffStatistics
	for _, d := range diffs {
		s := d.Statistics()
		stats.LinesAdded += s.LinesAdded
		stats.LinesRemoved += s.LinesRemoved
	}
	return stats, nil
}

// changeToFileDiff converts a go-git tree change to a model.FileDiff.
func changeToFileDiff(change *object.Change) model.FileDiff {
	action, err := change.Action()
	if err != nil {
		return model.FileDiff{Path: change.To.Name, Operation: model.DiffModified}
	}

	fd := model.FileDiff{}

	switch action {
	case 0: // Insert
		fd.Path = change.To.Name
		fd.Operation = model.DiffAdded
		fd.NewHash = change.To.TreeEntry.Hash.String()
	case 1: // Delete
		fd.Path = change.From.Name
		fd.Operation = model.DiffDeleted
		fd.OldHash = change.From.TreeEntry.Hash.String()
	case 2: // Modify
		fd.Path = change.To.Name
		fd.Operation = model.DiffModified
		fd.OldHash = change.From.TreeEntry.Hash.String()
		fd.NewHash = change.To.TreeEntry.Hash.String()
	}

	return fd
}
