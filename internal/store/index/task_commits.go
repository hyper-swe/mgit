package index

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/hyper-swe/mgit/internal/model"
)

// CommitRecord represents a row from the task_commits table.
// SandboxID is nil for commits produced outside a sandbox — a
// permanently visible provenance gap (FR-17.6, F-02). Refs: FR-4, FR-17.18
type CommitRecord struct {
	ID          int64   `json:"id"`
	TaskID      string  `json:"task_id"`
	CommitHash  string  `json:"commit_hash"`
	ContentHash string  `json:"content_hash"`
	AgentID     string  `json:"agent_id"`
	Position    int     `json:"position"`
	CreatedAt   string  `json:"created_at"`
	SandboxID   *string `json:"sandbox_id,omitempty"`
}

// TaskCommitInsert holds one task_commits row to append. An empty
// SandboxID stores NULL: the commit is recorded as unsandboxed.
// Refs: FR-4, FR-17.18
type TaskCommitInsert struct {
	TaskID      string
	CommitHash  string
	ContentHash string
	AgentID     string
	Position    int
	SandboxID   string // empty = unsandboxed (stored as NULL)
}

// insertTaskCommitTx inserts one task_commits row within an existing
// transaction. INSERT-only (append-only audit table, FR-12); sandbox_id
// is written once and never updated. now is the host-side timestamp.
// Shared by the single and batch appenders. Refs: FR-12, FR-17.18
func insertTaskCommitTx(ctx context.Context, tx *sql.Tx, in TaskCommitInsert, now string) error {
	const insertSQL = `INSERT INTO task_commits
		(task_id, commit_hash, content_hash, agent_id, position, created_at, sandbox_id)
		VALUES (?, ?, ?, ?, ?, ?, ?)`

	var sandboxID any // NULL when unsandboxed (queryable gap, F-02)
	if in.SandboxID != "" {
		sandboxID = in.SandboxID
	}
	if _, err := tx.ExecContext(ctx, insertSQL,
		in.TaskID, in.CommitHash, in.ContentHash, in.AgentID, in.Position, now, sandboxID); err != nil {
		return fmt.Errorf("insert task_commit %s: %w", in.CommitHash, err)
	}
	return nil
}

// AppendTaskCommit inserts one record into the task_commits table with
// sandbox provenance set at append time only. Refs: FR-4, FR-12, FR-17.18, MGIT-11.3.3
func (s *Store) AppendTaskCommit(ctx context.Context, in TaskCommitInsert) error {
	return s.WriteTx(ctx, func(tx *sql.Tx) error {
		return insertTaskCommitTx(ctx, tx, in, s.clock().UTC().Format(time.RFC3339))
	})
}

// AppendTaskCommits inserts multiple records in a SINGLE serialized
// transaction: every row commits or none do (sandbox land is
// all-or-nothing, FR-17.5; a failure mid-batch leaves no partial land).
// All rows share one host receive-time (FR-17.28). INSERT-only per FR-12.
// Refs: FR-17.5, FR-17.18, MGIT-11.8.5
func (s *Store) AppendTaskCommits(ctx context.Context, ins []TaskCommitInsert) error {
	if len(ins) == 0 {
		return nil
	}
	now := s.clock().UTC().Format(time.RFC3339)
	return s.WriteTx(ctx, func(tx *sql.Tx) error {
		for _, in := range ins {
			if err := insertTaskCommitTx(ctx, tx, in, now); err != nil {
				return err
			}
		}
		return nil
	})
}

// AddCommitToTask inserts an unsandboxed record into task_commits.
// Kept for pre-FR-17 callers; new code should use AppendTaskCommit.
// Refs: FR-4, FR-12, MGIT-2.3.3
func (s *Store) AddCommitToTask(ctx context.Context, taskID, commitHash, contentHash, agentID string, position int) error {
	return s.AppendTaskCommit(ctx, TaskCommitInsert{
		TaskID: taskID, CommitHash: commitHash, ContentHash: contentHash,
		AgentID: agentID, Position: position,
	})
}

// GetTaskCommits returns all commits for a task, ordered by position.
// Refs: FR-4 (task -> commits query)
func (s *Store) GetTaskCommits(ctx context.Context, taskID string) ([]CommitRecord, error) {
	// Parameterized query for task -> commits lookup
	const querySQL = `SELECT id, task_id, commit_hash, content_hash, agent_id, position, created_at, sandbox_id
		FROM task_commits WHERE task_id = ? ORDER BY position ASC`

	return s.queryCommitRecords(ctx, querySQL, taskID)
}

// GetUnsandboxedCommits returns every commit recorded without sandbox
// provenance (sandbox_id IS NULL) — the permanently visible gap that
// require_sandbox closes (FR-17.6, F-02). The result is unpaginated:
// callers presenting large audit reports should add LIMIT/OFFSET
// support before exposing this through the CLI. Refs: FR-17.18
func (s *Store) GetUnsandboxedCommits(ctx context.Context) ([]CommitRecord, error) {
	// NULL sandbox_id = unsandboxed commit (provenance gap query)
	const querySQL = `SELECT id, task_id, commit_hash, content_hash, agent_id, position, created_at, sandbox_id
		FROM task_commits WHERE sandbox_id IS NULL ORDER BY id ASC`

	return s.queryCommitRecords(ctx, querySQL)
}

// queryCommitRecords runs one parameterized task_commits SELECT whose
// column list matches CommitRecord, scanning sandbox_id as nullable.
func (s *Store) queryCommitRecords(ctx context.Context, querySQL string, args ...any) ([]CommitRecord, error) {
	var records []CommitRecord
	err := s.ReadTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, querySQL, args...)
		if err != nil {
			return fmt.Errorf("query task commits: %w", err)
		}
		defer rows.Close() //nolint:errcheck // rows.Close error is non-critical after successful read

		for rows.Next() {
			var r CommitRecord
			var sandboxID sql.NullString
			if err := rows.Scan(&r.ID, &r.TaskID, &r.CommitHash, &r.ContentHash,
				&r.AgentID, &r.Position, &r.CreatedAt, &sandboxID); err != nil {
				return fmt.Errorf("scan task commit: %w", err)
			}
			if sandboxID.Valid {
				r.SandboxID = &sandboxID.String
			}
			records = append(records, r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return records, nil
}

// GetCommitTask finds which task owns a commit (reverse mapping).
// Returns ErrTaskNotFound if no mapping exists.
// Refs: FR-4 (commit -> task query)
func (s *Store) GetCommitTask(ctx context.Context, commitHash string) (string, error) {
	taskID, _, err := s.GetCommitProvenance(ctx, commitHash)
	return taskID, err
}

// GetCommitProvenance returns the indexed task_id and authoritative ADR-002
// content_hash (SHA-256) for a commit hash. This is the read-path join that
// lets show/log surface a commit's provenance — the content_hash is recorded
// at create time and cannot be recomputed from the git object alone (it covers
// the file diffs, which the object does not carry). Returns ErrTaskNotFound if
// the commit was never indexed. Refs: FR-4, ADR-002, MGIT-19
func (s *Store) GetCommitProvenance(ctx context.Context, commitHash string) (taskID, contentHash string, err error) {
	// Parameterized commit -> (task_id, content_hash) reverse lookup.
	const querySQL = `SELECT task_id, content_hash FROM task_commits WHERE commit_hash = ? LIMIT 1`

	err = s.ReadTx(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, querySQL, commitHash).Scan(&taskID, &contentHash)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", fmt.Errorf("%w: no task for commit %s", model.ErrTaskNotFound, commitHash)
	}
	if err != nil {
		return "", "", fmt.Errorf("query commit provenance %s: %w", commitHash, err)
	}
	return taskID, contentHash, nil
}

// DeleteFromTask always returns ErrAppendOnlyViolation.
// The task_commits table is INSERT-only per FR-12.
// Refs: FR-12
func (s *Store) DeleteFromTask(_ context.Context, _, _ string) error {
	return fmt.Errorf("%w: task_commits is append-only", model.ErrAppendOnlyViolation)
}
