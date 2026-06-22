package service

import (
	"context"
	"fmt"

	"github.com/hyper-swe/mgit/internal/model"
	gitstore "github.com/hyper-swe/mgit/internal/store/git"
	"github.com/hyper-swe/mgit/internal/store/index"
)

// RollbackRequest holds parameters for rolling back task commits.
// Refs: FR-6
type RollbackRequest struct {
	TaskID string `json:"task_id"`
	Reason string `json:"reason,omitempty"`
	DryRun bool   `json:"dry_run,omitempty"`
}

// RollbackService creates revert commits to undo task changes.
// Original commits are never deleted — append-only per FR-12.
// Refs: FR-6, MGIT-3.1.3
type RollbackService struct {
	commitStore *gitstore.CommitStore
	indexStore  *index.Store
	repo        *gitstore.Repository
	audit       *AuditService
}

// NewRollbackService creates a RollbackService with injected dependencies.
func NewRollbackService(repo *gitstore.Repository, cs *gitstore.CommitStore, idx *index.Store) *RollbackService {
	return &RollbackService{
		commitStore: cs,
		indexStore:  idx,
		repo:        repo,
	}
}

// WithAudit attaches an AuditService so successful rollbacks are recorded in the
// append-only audit trail surfaced by `mgit audit`. Returns the receiver for
// fluent wiring. If unset, rollbacks proceed without an audit entry.
// Refs: FR-12, MGIT-20
func (s *RollbackService) WithAudit(a *AuditService) *RollbackService {
	s.audit = a
	return s
}

// RollbackTask creates a revert commit that undoes all changes from a task.
// Original commits remain in history (append-only).
// If DryRun is true, returns what would happen without making changes.
// Refs: FR-6, FR-12
func (s *RollbackService) RollbackTask(ctx context.Context, req RollbackRequest) (*model.Commit, error) {
	// Get all commits for this task
	records, err := s.indexStore.GetTaskCommits(ctx, req.TaskID)
	if err != nil {
		return nil, fmt.Errorf("rollback task %s: get commits: %w", req.TaskID, err)
	}
	if len(records) == 0 {
		return nil, fmt.Errorf("rollback task %s: %w", req.TaskID, model.ErrTaskNotFound)
	}

	// Collect all file diffs and compute inverse
	var allDiffs []model.FileDiff
	for _, rec := range records {
		c, getErr := s.commitStore.GetCommit(ctx, rec.CommitHash)
		if getErr != nil {
			return nil, fmt.Errorf("rollback task %s: get commit %s: %w", req.TaskID, rec.CommitHash, getErr)
		}
		allDiffs = append(allDiffs, c.FileDiffs...)
	}

	inverseDiffs := invertDiffs(allDiffs)

	// Build revert message
	reason := req.Reason
	if reason == "" {
		reason = "rollback"
	}
	message := fmt.Sprintf("[MGIT:%s] Revert: %s (%d commits)", req.TaskID, reason, len(records))

	taskID, err := model.ParseTaskID(req.TaskID)
	if err != nil {
		return nil, fmt.Errorf("rollback task: %w", err)
	}

	revertCommit := &model.Commit{
		TaskID:     taskID,
		AgentID:    "mgit-rollback",
		Message:    message,
		FileDiffs:  inverseDiffs,
		CommitType: model.CommitTypeRollback,
		CreatedBy:  "mgit-rollback",
		Branch:     model.TaskBranchName(req.TaskID),
	}

	if req.DryRun {
		return revertCommit, nil
	}

	// Create the revert commit (append-only: original commits remain)
	hash, err := s.commitStore.CreateCommit(ctx, revertCommit)
	if err != nil {
		return nil, fmt.Errorf("rollback task %s: create revert commit: %w", req.TaskID, err)
	}

	// Index the revert commit
	position := len(records)
	err = s.indexStore.AddCommitToTask(ctx, req.TaskID, hash, revertCommit.ContentHash, "mgit-rollback", position)
	if err != nil {
		return nil, fmt.Errorf("rollback task %s: index revert commit: %w", req.TaskID, err)
	}

	// Record the operation in the append-only audit trail (MGIT-20).
	if err := s.logAudit(revertCommit, req.TaskID, reason); err != nil {
		return nil, fmt.Errorf("rollback task %s: audit: %w", req.TaskID, err)
	}

	return revertCommit, nil
}

// logAudit appends a ROLLBACK entry to the audit trail. No-op when no
// AuditService is wired. Refs: FR-12, MGIT-20
func (s *RollbackService) logAudit(c *model.Commit, taskID, reason string) error {
	if s.audit == nil {
		return nil
	}
	return s.audit.LogOperation(AuditEntry{
		Operation: AuditRollback,
		AgentID:   c.AgentID,
		TaskID:    taskID,
		CommitID:  c.CommitID,
		Details:   reason,
	})
}

// invertDiffs computes the inverse of a set of file diffs.
// Added files become deleted, deleted become added, modified swap hashes.
func invertDiffs(diffs []model.FileDiff) []model.FileDiff {
	inverse := make([]model.FileDiff, 0, len(diffs))
	for _, d := range diffs {
		inv := model.FileDiff{
			Path:    d.Path,
			OldHash: d.NewHash,
			NewHash: d.OldHash,
		}
		switch d.Operation {
		case model.DiffAdded:
			inv.Operation = model.DiffDeleted
		case model.DiffDeleted:
			inv.Operation = model.DiffAdded
		default:
			inv.Operation = model.DiffModified
		}
		inverse = append(inverse, inv)
	}
	return inverse
}
