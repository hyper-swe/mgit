package model

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"
)

// CommitType identifies the nature of a commit.
// Refs: FR-3
type CommitType string

// Supported commit types per FR-3.
const (
	CommitTypeNormal   CommitType = "normal"
	CommitTypeRollback CommitType = "rollback"
	CommitTypeSquash   CommitType = "squash"
	CommitTypeMerge    CommitType = "merge"
	CommitTypeSystem   CommitType = "system"
)

// shortIDLen is the length of the human-readable short commit ID.
const shortIDLen = 8

// Commit represents a task-tagged micro-commit.
// Task ID is stored in the commit message and indexed in SQLite
// for O(1) lookup by task. All commits are append-only.
// Contains all 16 fields per FR-3 Commit Data Model.
// Refs: FR-2, FR-3, NFR-1.1, ADR-002
type Commit struct {
	// Identity
	CommitID    string `json:"commit_id"`    // SHA-256 content-addressed unique ID
	ContentHash string `json:"content_hash"` // SHA-256 integrity hash (authoritative per ADR-002)

	// Lineage
	ParentID string `json:"parent_id,omitempty"` // Parent commit SHA-256 (empty for first commit)
	TreeHash string `json:"tree_hash"`           // File tree hash

	// Task
	TaskID TaskID `json:"task_id"` // mtix dot-notation task ID

	// Agent
	AgentID   string `json:"agent_id"`             // Creator agent ID
	SessionID string `json:"session_id,omitempty"` // mtix session ID

	// Content
	Message   string     `json:"message"`              // Commit message with [MGIT:TASK_ID] prefix
	FileDiffs []FileDiff `json:"file_diffs,omitempty"` // Array of file changes

	// Time
	CreatedAt time.Time `json:"created_at"` // Creation timestamp (ISO-8601 UTC)

	// Metadata
	Metadata  map[string]any `json:"metadata,omitempty"`  // Extensible key-value pairs
	Signature string         `json:"signature,omitempty"` // Optional Ed25519 signature

	// Classification
	CommitType CommitType `json:"commit_type"` // normal, rollback, squash, merge, system
	CreatedBy  string     `json:"created_by"`  // Agent that created this commit
	Branch     string     `json:"branch"`      // Branch this commit belongs to
}

// ComputeContentHash returns the mgit SHA-256 integrity hash (ADR-002):
// SHA-256(message + file_diffs_json + task_id + parent + created_at). It
// is the single, authoritative definition of content_hash, shared by
// commit creation (store/git) and sandbox land re-verification
// (FR-17.5/17.24) so the two can never disagree on a commit's identity.
// Refs: ADR-002, FR-3.1, FR-17.24, NFR-5
func (c *Commit) ComputeContentHash() string {
	h := sha256.New()
	h.Write([]byte(c.Message))
	diffsJSON, _ := json.Marshal(c.FileDiffs) //nolint:errcheck // marshal of known type
	h.Write(diffsJSON)
	h.Write([]byte(c.TaskID.String()))
	h.Write([]byte(c.ParentID))
	h.Write([]byte(c.CreatedAt.UTC().Format("2006-01-02T15:04:05Z")))
	return fmt.Sprintf("%x", h.Sum(nil))
}

// Validate checks that the Commit has all required fields populated.
// Returns ErrInvalidCommit if any required field is missing.
// Refs: FR-3
func (c Commit) Validate() error {
	if c.CommitID == "" {
		return fmt.Errorf("%w: commit_id must not be empty", ErrInvalidCommit)
	}
	if c.TaskID.IsZero() {
		return fmt.Errorf("%w: task_id must not be empty", ErrInvalidCommit)
	}
	if c.CreatedAt.IsZero() {
		return fmt.Errorf("%w: created_at must not be zero", ErrInvalidCommit)
	}
	if c.Message == "" {
		return fmt.Errorf("%w: message must not be empty", ErrInvalidCommit)
	}
	return nil
}

// String returns a human-readable representation for logging.
func (c Commit) String() string {
	return fmt.Sprintf("%s [%s] %s", c.ShortID(), c.TaskID.String(), c.Message)
}

// ShortID returns the first 8 characters of the commit ID.
// Refs: FR-3 (short_id field)
func (c Commit) ShortID() string {
	if len(c.CommitID) < shortIDLen {
		return c.CommitID
	}
	return c.CommitID[:shortIDLen]
}
