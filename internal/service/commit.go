// Package service implements the business logic layer for mgit.
// Services orchestrate go-git and SQLite stores. Handlers (CLI, API, MCP)
// call services — never stores directly.
// Refs: FR-2, FR-3, MGIT-3.1.1
package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/hyper-swe/mgit/internal/model"
	gitstore "github.com/hyper-swe/mgit/internal/store/git"
	"github.com/hyper-swe/mgit/internal/store/index"
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
	audit       *AuditService
}

// NewCommitService creates a CommitService with injected dependencies.
func NewCommitService(repo *gitstore.Repository, cs *gitstore.CommitStore, idx *index.Store) *CommitService {
	return &CommitService{
		commitStore: cs,
		indexStore:  idx,
		repo:        repo,
	}
}

// WithAudit attaches an AuditService so successful commits are recorded in the
// append-only audit trail surfaced by `mgit audit`. Returns the receiver for
// fluent wiring. If unset, commits proceed without an audit entry.
// Refs: FR-12, MGIT-20
func (s *CommitService) WithAudit(a *AuditService) *CommitService {
	s.audit = a
	return s
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

	// Record the operation in the append-only audit trail (MGIT-20).
	if err := s.logAudit(commit); err != nil {
		return nil, fmt.Errorf("audit commit: %w", err)
	}

	return commit, nil
}

// logAudit appends a CREATE_COMMIT entry to the audit trail. No-op when no
// AuditService is wired. Refs: FR-12, MGIT-20
func (s *CommitService) logAudit(c *model.Commit) error {
	if s.audit == nil {
		return nil
	}
	return s.audit.LogOperation(AuditEntry{
		Operation: AuditCreateCommit,
		AgentID:   c.AgentID,
		TaskID:    c.TaskID.String(),
		CommitID:  c.CommitID,
	})
}

// GetCommit retrieves a commit by hash (full or unambiguous abbreviated
// prefix) and enriches it with authoritative provenance from the index: the
// ADR-002 content_hash recorded at create time, plus the task_id as a fallback
// when the git message carried no [MGIT:] tag. show/log/cherry-pick rely on
// these fields being populated. Refs: FR-3, FR-4, ADR-002, MGIT-18, MGIT-19
func (s *CommitService) GetCommit(ctx context.Context, hash string) (*model.Commit, error) {
	c, err := s.commitStore.GetCommit(ctx, hash)
	if err != nil {
		return nil, err
	}
	if err := s.enrichProvenance(ctx, c); err != nil {
		return nil, err
	}
	return c, nil
}

// ListCommits returns all commits reachable from HEAD, each enriched with its
// indexed content_hash and task_id so `mgit log` surfaces full provenance.
// Refs: FR-3, FR-4, ADR-002, MGIT-19
func (s *CommitService) ListCommits(ctx context.Context) ([]*model.Commit, error) {
	commits, err := s.commitStore.ListCommits(ctx)
	if err != nil {
		return nil, err
	}
	for _, c := range commits {
		if err := s.enrichProvenance(ctx, c); err != nil {
			return nil, err
		}
	}
	return commits, nil
}

// enrichProvenance binds a commit's authoritative ADR-002 content_hash (and,
// when missing, its task_id) from the SQLite index. A commit simply absent
// from the index (ErrTaskNotFound — e.g. mgit's own initial system commit) is
// left as-is and is not an error. A genuine index/DB failure IS propagated:
// on this audit/provenance read path, silently returning blank provenance for
// a corrupt index would hide an integrity problem. The content_hash cannot be
// recomputed from the git object alone (it covers the file diffs the object
// does not carry), so the index is its sole authoritative source on read.
// Refs: FR-4, ADR-002, MGIT-19
func (s *CommitService) enrichProvenance(ctx context.Context, c *model.Commit) error {
	taskID, contentHash, err := s.indexStore.GetCommitProvenance(ctx, c.CommitID)
	if errors.Is(err, model.ErrTaskNotFound) {
		return nil // not indexed (e.g. initial system commit); leave parsed fields
	}
	if err != nil {
		return fmt.Errorf("enrich provenance for %s: %w", c.CommitID, err)
	}
	c.ContentHash = contentHash
	if c.TaskID.IsZero() {
		if tid, perr := model.ParseTaskID(taskID); perr == nil {
			c.TaskID = tid
		}
	}
	return nil
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
