package model

import (
	"context"
	"fmt"
	"path/filepath"
	"time"
)

// WorktreeInfo represents a linked worktree for multi-agent development.
// Each worktree is bound to exactly one task and one branch.
// Refs: FR-16, MGIT-8.1.1
type WorktreeInfo struct {
	Path         string    `json:"path"`
	Name         string    `json:"name"`
	Branch       string    `json:"branch"`
	TaskID       string    `json:"task_id"`
	AgentID      string    `json:"agent_id,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	LastCommitAt time.Time `json:"last_commit_at,omitempty"`
}

// Validate checks that the WorktreeInfo has required fields.
// Refs: FR-16
func (w WorktreeInfo) Validate() error {
	if w.Path == "" {
		return &ValidationError{Field: "path", Message: "must not be empty"}
	}
	if w.TaskID == "" {
		return &ValidationError{Field: "task_id", Message: "must not be empty"}
	}
	if _, err := ParseTaskID(w.TaskID); err != nil {
		return &ValidationError{Field: "task_id", Message: fmt.Sprintf("invalid format: %s", w.TaskID)}
	}
	return nil
}

// DeriveNameFromPath extracts the worktree name from its filesystem path.
func DeriveNameFromPath(path string) string {
	return filepath.Base(path)
}

// WorktreeAddOptions holds parameters for creating a new worktree.
// Refs: FR-16
type WorktreeAddOptions struct {
	Path    string `json:"path"`
	TaskID  string `json:"task_id"`
	AgentID string `json:"agent_id,omitempty"`
	Branch  string `json:"branch,omitempty"`
}

// Validate checks the add options.
func (o WorktreeAddOptions) Validate() error {
	if o.Path == "" {
		return &ValidationError{Field: "path", Message: "must not be empty"}
	}
	if o.TaskID == "" {
		return &ValidationError{Field: "task_id", Message: "must not be empty"}
	}
	if _, err := ParseTaskID(o.TaskID); err != nil {
		return &ValidationError{Field: "task_id", Message: fmt.Sprintf("invalid format: %s", o.TaskID)}
	}
	return nil
}

// WorktreeManager defines the pluggable interface for worktree operations.
// v1 is mgit-managed (filesystem + SQLite). v2 may use go-git v6 native.
// Refs: FR-16.10, ADR-004
type WorktreeManager interface {
	Add(ctx context.Context, opts WorktreeAddOptions) (*WorktreeInfo, error)
	List(ctx context.Context) ([]WorktreeInfo, error)
	Remove(ctx context.Context, path string, force bool) error
	Prune(ctx context.Context, dryRun bool) ([]string, error)
	Resolve(ctx context.Context, path string) (*WorktreeInfo, error)
}
