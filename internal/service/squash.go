package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/astutic/mgit/internal/model"
	gitstore "github.com/astutic/mgit/internal/store/git"
	"github.com/astutic/mgit/internal/store/index"
)

// SquashRequest holds parameters for squashing task commits.
// Refs: FR-7
type SquashRequest struct {
	TaskID  string `json:"task_id"`
	Message string `json:"message,omitempty"`
	DryRun  bool   `json:"dry_run,omitempty"`
}

// SquashService consolidates micro-commits for a task into a single commit.
// Refs: FR-7, MGIT-3.1.2
type SquashService struct {
	commitStore *gitstore.CommitStore
	indexStore  *index.Store
	repo        *gitstore.Repository
}

// NewSquashService creates a SquashService with injected dependencies.
func NewSquashService(repo *gitstore.Repository, cs *gitstore.CommitStore, idx *index.Store) *SquashService {
	return &SquashService{
		commitStore: cs,
		indexStore:  idx,
		repo:        repo,
	}
}

// SquashTask consolidates all commits for a task into a single squash commit.
// Original commits remain in history (append-only per FR-12).
// Refs: FR-7
func (s *SquashService) SquashTask(ctx context.Context, req SquashRequest) (*model.Commit, error) {
	// Retrieve all commits for this task
	records, err := s.indexStore.GetTaskCommits(ctx, req.TaskID)
	if err != nil {
		return nil, fmt.Errorf("squash task %s: get commits: %w", req.TaskID, err)
	}
	if len(records) == 0 {
		return nil, fmt.Errorf("squash task %s: %w", req.TaskID, model.ErrTaskNotFound)
	}

	// Merge all file diffs from individual commits
	var allDiffs []model.FileDiff
	var commitSummaries []string
	for _, rec := range records {
		c, getErr := s.commitStore.GetCommit(ctx, rec.CommitHash)
		if getErr != nil {
			return nil, fmt.Errorf("squash task %s: get commit %s: %w", req.TaskID, rec.CommitHash, getErr)
		}
		allDiffs = append(allDiffs, c.FileDiffs...)
		commitSummaries = append(commitSummaries,
			fmt.Sprintf("- %s: %s", c.ShortID(), c.Message))
	}

	// Merge diffs: last write wins per path
	mergedDiffs := mergeDiffs(allDiffs)

	// Build squash message
	message := req.Message
	if message == "" {
		message = fmt.Sprintf("[%s] Squashed from %d micro-commits", req.TaskID, len(records))
	}
	if len(commitSummaries) > 0 {
		message = message + "\n\n" + strings.Join(commitSummaries, "\n")
	}

	if req.DryRun {
		// Return what would be created without making changes
		taskID, parseErr := model.ParseTaskID(req.TaskID)
		if parseErr != nil {
			return nil, fmt.Errorf("squash task: %w", parseErr)
		}
		return &model.Commit{
			TaskID:     taskID,
			Message:    message,
			FileDiffs:  mergedDiffs,
			CommitType: model.CommitTypeSquash,
		}, nil
	}

	// Create the squash commit
	taskID, err := model.ParseTaskID(req.TaskID)
	if err != nil {
		return nil, fmt.Errorf("squash task: %w", err)
	}

	squashCommit := &model.Commit{
		TaskID:     taskID,
		AgentID:    "mgit-squash",
		Message:    message,
		FileDiffs:  mergedDiffs,
		CommitType: model.CommitTypeSquash,
		CreatedBy:  "mgit-squash",
		Branch:     "task/" + req.TaskID,
	}

	hash, err := s.commitStore.CreateCommit(ctx, squashCommit)
	if err != nil {
		return nil, fmt.Errorf("squash task %s: create squash commit: %w", req.TaskID, err)
	}

	// Index the squash commit
	position := len(records)
	err = s.indexStore.AddCommitToTask(ctx, req.TaskID, hash, squashCommit.ContentHash, "mgit-squash", position)
	if err != nil {
		return nil, fmt.Errorf("squash task %s: index squash commit: %w", req.TaskID, err)
	}

	return squashCommit, nil
}

// mergeDiffs consolidates file diffs, keeping the last operation per path.
func mergeDiffs(diffs []model.FileDiff) []model.FileDiff {
	seen := make(map[string]model.FileDiff)
	var order []string

	for _, d := range diffs {
		if _, exists := seen[d.Path]; !exists {
			order = append(order, d.Path)
		}
		seen[d.Path] = d
	}

	merged := make([]model.FileDiff, 0, len(order))
	for _, path := range order {
		merged = append(merged, seen[path])
	}
	return merged
}
