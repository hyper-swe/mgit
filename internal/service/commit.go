// Package service implements the business logic layer for mgit.
// Services orchestrate go-git and SQLite stores. Handlers (CLI, API, MCP)
// call services — never stores directly.
// Refs: FR-2, FR-3, MGIT-3.1.1
package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/hyper-swe/mgit-dev/internal/model"
	gitstore "github.com/hyper-swe/mgit-dev/internal/store/git"
	"github.com/hyper-swe/mgit-dev/internal/store/index"
)

// CreateCommitRequest holds the parameters for creating a new commit.
// Refs: FR-2, FR-3
type CreateCommitRequest struct {
	TaskID    string           `json:"task_id"`
	AgentID   string           `json:"agent_id"`
	SessionID string           `json:"session_id,omitempty"`
	Message   string           `json:"message,omitempty"`
	FileDiffs []model.FileDiff `json:"file_diffs,omitempty"`
	Branch    string           `json:"branch,omitempty"`
}

// CommitService orchestrates commit creation across go-git and SQLite.
// Refs: FR-2, FR-3, MGIT-3.1.1
type CommitService struct {
	commitStore *gitstore.CommitStore
	indexStore  *index.Store
	repo        *gitstore.Repository
}

// NewCommitService creates a CommitService with injected dependencies.
func NewCommitService(repo *gitstore.Repository, cs *gitstore.CommitStore, idx *index.Store) *CommitService {
	return &CommitService{
		commitStore: cs,
		indexStore:  idx,
		repo:        repo,
	}
}

// CreateCommit creates a new micro-commit, storing it in both go-git
// and the SQLite index. Auto-generates a message if none provided.
// Refs: FR-2, FR-3, ADR-002
func (s *CommitService) CreateCommit(ctx context.Context, req CreateCommitRequest) (*model.Commit, error) {
	// Validate task ID
	taskID, err := model.ParseTaskID(req.TaskID)
	if err != nil {
		return nil, fmt.Errorf("create commit: %w", err)
	}

	// Auto-generate message if not provided
	message := req.Message
	if message == "" {
		message = autoMessage(req.TaskID, req.FileDiffs)
	}
	// Prefix with task ID per FR-2
	if !strings.HasPrefix(message, "[MGIT:") {
		message = fmt.Sprintf("[MGIT:%s] %s", req.TaskID, message)
	}

	// Build the commit model
	commit := &model.Commit{
		TaskID:     taskID,
		AgentID:    req.AgentID,
		SessionID:  req.SessionID,
		Message:    message,
		FileDiffs:  req.FileDiffs,
		CommitType: model.CommitTypeNormal,
		CreatedBy:  req.AgentID,
		Branch:     req.Branch,
	}

	// Store in go-git (sets CommitID, ContentHash, CreatedAt, ParentID)
	hash, err := s.commitStore.CreateCommit(ctx, commit)
	if err != nil {
		return nil, fmt.Errorf("create commit in git: %w", err)
	}

	// Get position for this task (number of existing commits)
	existing, err := s.indexStore.GetTaskCommits(ctx, req.TaskID)
	if err != nil {
		return nil, fmt.Errorf("get existing commits: %w", err)
	}
	position := len(existing)

	// Store in SQLite index
	err = s.indexStore.AddCommitToTask(ctx, req.TaskID, hash, commit.ContentHash, req.AgentID, position)
	if err != nil {
		return nil, fmt.Errorf("index commit: %w", err)
	}

	return commit, nil
}

// GetCommit retrieves a commit by hash.
// Refs: FR-3
func (s *CommitService) GetCommit(ctx context.Context, hash string) (*model.Commit, error) {
	return s.commitStore.GetCommit(ctx, hash)
}

// ListCommits returns all commits reachable from HEAD.
// Refs: FR-3
func (s *CommitService) ListCommits(ctx context.Context) ([]*model.Commit, error) {
	return s.commitStore.ListCommits(ctx)
}

// GetTaskCommits returns all commits for a given task ID.
// Refs: FR-4
func (s *CommitService) GetTaskCommits(ctx context.Context, taskID string) ([]index.CommitRecord, error) {
	return s.indexStore.GetTaskCommits(ctx, taskID)
}

// autoMessage generates a commit message from file diffs.
func autoMessage(taskID string, diffs []model.FileDiff) string {
	if len(diffs) == 0 {
		return fmt.Sprintf("Task %s: empty commit", taskID)
	}
	if len(diffs) == 1 {
		return fmt.Sprintf("%s %s", diffs[0].Operation, diffs[0].Path)
	}
	return fmt.Sprintf("%d files changed", len(diffs))
}
