package model

import (
	"fmt"
	"time"
)

// Branch represents a task-scoped branch in the mgit repository.
// Each branch is owned by exactly one task ID. Branches support
// advisory locking for concurrent agent safety.
// Refs: FR-5, MGIT-2.1.2
type Branch struct {
	BranchID    string    `json:"branch_id"`              // Unique branch identifier
	Name        string    `json:"name"`                   // Branch name (e.g., task/MGIT-1.2)
	HeadCommit  string    `json:"head_commit"`            // Current HEAD commit ID
	TaskID      TaskID    `json:"task_id"`                // Owning task ID
	CreatedAt   time.Time `json:"created_at"`             // Branch creation timestamp
	LockedBy    string    `json:"locked_by,omitempty"`    // Agent holding the lock
	LockedUntil time.Time `json:"locked_until,omitempty"` // Lock expiry time
	MergedTo    string    `json:"merged_to,omitempty"`    // Target branch if merged
	IsMerged    bool      `json:"is_merged"`              // Whether branch has been merged
}

// Lock acquires an advisory lock on the branch for the given agent.
// If the branch is already locked by a different agent and the lock
// has not expired, returns ErrBranchLocked.
// Refs: FR-5, NFR-3
func (b *Branch) Lock(agentID string, now time.Time, duration time.Duration) error {
	if b.IsLocked(now) && b.LockedBy != agentID {
		return fmt.Errorf("%w: held by %s until %s",
			ErrBranchLocked, b.LockedBy, b.LockedUntil.Format(time.RFC3339))
	}
	b.LockedBy = agentID
	b.LockedUntil = now.Add(duration)
	return nil
}

// Unlock releases the advisory lock on the branch.
func (b *Branch) Unlock() {
	b.LockedBy = ""
	b.LockedUntil = time.Time{}
}

// IsLocked returns true if the branch is locked and the lock has not expired.
func (b *Branch) IsLocked(now time.Time) bool {
	return b.LockedBy != "" && now.Before(b.LockedUntil)
}

// Validate checks that the branch has required fields populated.
// Refs: FR-5
func (b Branch) Validate() error {
	if b.Name == "" {
		return &ValidationError{Field: "name", Message: "must not be empty"}
	}
	if b.TaskID.IsZero() {
		return &ValidationError{Field: "task_id", Message: "must not be empty"}
	}
	return nil
}

// String returns a human-readable representation for logging.
func (b Branch) String() string {
	return fmt.Sprintf("%s [%s] head=%s", b.Name, b.TaskID.String(), b.HeadCommit)
}
