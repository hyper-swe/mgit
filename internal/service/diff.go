package service

import (
	"context"
	"fmt"

	"github.com/astutic/mgit/internal/model"
	gitstore "github.com/astutic/mgit/internal/store/git"
	"github.com/astutic/mgit/internal/store/index"
)

// DiffService computes diffs between commits and across tasks.
// Refs: FR-11, MGIT-3.2.1
type DiffService struct {
	diffStore   *gitstore.DiffStore
	commitStore *gitstore.CommitStore
	indexStore  *index.Store
}

// NewDiffService creates a DiffService with injected dependencies.
func NewDiffService(ds *gitstore.DiffStore, cs *gitstore.CommitStore, idx *index.Store) *DiffService {
	return &DiffService{
		diffStore:   ds,
		commitStore: cs,
		indexStore:  idx,
	}
}

// DiffCommits computes the file-level diff between two commits.
// Refs: FR-11
func (s *DiffService) DiffCommits(ctx context.Context, fromHash, toHash string) ([]model.FileDiff, error) {
	diffs, err := s.diffStore.DiffCommits(ctx, fromHash, toHash)
	if err != nil {
		return nil, fmt.Errorf("diff commits: %w", err)
	}
	return diffs, nil
}

// DiffTask computes the cumulative diff for all commits in a task.
// Returns the diff from before the task's first commit to after its last.
// Refs: FR-11
func (s *DiffService) DiffTask(ctx context.Context, taskID string) ([]model.FileDiff, error) {
	records, err := s.indexStore.GetTaskCommits(ctx, taskID)
	if err != nil {
		return nil, fmt.Errorf("diff task %s: get commits: %w", taskID, err)
	}
	if len(records) == 0 {
		return nil, fmt.Errorf("diff task %s: %w", taskID, model.ErrTaskNotFound)
	}

	// Get the first commit's parent as the "before" state
	firstCommit, err := s.commitStore.GetCommit(ctx, records[0].CommitHash)
	if err != nil {
		return nil, fmt.Errorf("diff task %s: get first commit: %w", taskID, err)
	}

	lastHash := records[len(records)-1].CommitHash

	if firstCommit.ParentID == "" {
		// First commit has no parent — return empty diff
		return []model.FileDiff{}, nil
	}

	return s.diffStore.DiffCommits(ctx, firstCommit.ParentID, lastHash)
}

// DiffRange computes the diff over a range of commits.
// Refs: FR-11
func (s *DiffService) DiffRange(ctx context.Context, fromHash, toHash string) ([]model.FileDiff, error) {
	return s.diffStore.DiffCommits(ctx, fromHash, toHash)
}

// Statistics computes aggregate line change counts for a set of diffs.
// Refs: FR-11
func (s *DiffService) Statistics(diffs []model.FileDiff) model.DiffStatistics {
	var stats model.DiffStatistics
	for _, d := range diffs {
		s := d.Statistics()
		stats.LinesAdded += s.LinesAdded
		stats.LinesRemoved += s.LinesRemoved
	}
	return stats
}
