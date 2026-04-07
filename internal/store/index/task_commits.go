package index

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/astutic/mgit/internal/model"
)

// CommitRecord represents a row from the task_commits table.
// Refs: FR-4
type CommitRecord struct {
	ID          int64  `json:"id"`
	TaskID      string `json:"task_id"`
	CommitHash  string `json:"commit_hash"`
	ContentHash string `json:"content_hash"`
	AgentID     string `json:"agent_id"`
	Position    int    `json:"position"`
	CreatedAt   string `json:"created_at"`
}

// AddCommitToTask inserts a record into the task_commits table.
// Returns an error if the (task_id, commit_hash) pair already exists.
// This is an INSERT-only operation per FR-12 append-only requirement.
// Refs: FR-4, FR-12, MGIT-2.3.3
func (s *Store) AddCommitToTask(ctx context.Context, taskID, commitHash, contentHash, agentID string, position int) error {
	// INSERT-only: append-only audit table (FR-12)
	const insertSQL = `INSERT INTO task_commits (task_id, commit_hash, content_hash, agent_id, position, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`

	return s.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, insertSQL,
			taskID, commitHash, contentHash, agentID, position,
			s.clock().UTC().Format(time.RFC3339))
		if err != nil {
			return fmt.Errorf("insert task_commit: %w", err)
		}
		return nil
	})
}

// GetTaskCommits returns all commits for a task, ordered by position.
// Refs: FR-4 (task -> commits query)
func (s *Store) GetTaskCommits(ctx context.Context, taskID string) ([]CommitRecord, error) {
	// Parameterized query for task -> commits lookup
	const querySQL = `SELECT id, task_id, commit_hash, content_hash, agent_id, position, created_at
		FROM task_commits WHERE task_id = ? ORDER BY position ASC`

	var records []CommitRecord
	err := s.ReadTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, querySQL, taskID)
		if err != nil {
			return fmt.Errorf("query task commits: %w", err)
		}
		defer rows.Close() //nolint:errcheck // rows.Close error is non-critical after successful read

		for rows.Next() {
			var r CommitRecord
			if err := rows.Scan(&r.ID, &r.TaskID, &r.CommitHash, &r.ContentHash, &r.AgentID, &r.Position, &r.CreatedAt); err != nil {
				return fmt.Errorf("scan task commit: %w", err)
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
	// Parameterized query for commit -> task reverse lookup
	const querySQL = `SELECT task_id FROM task_commits WHERE commit_hash = ? LIMIT 1`

	var taskID string
	err := s.ReadTx(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, querySQL, commitHash).Scan(&taskID)
	})
	if err != nil {
		return "", fmt.Errorf("%w: no task for commit %s", model.ErrTaskNotFound, commitHash)
	}
	return taskID, nil
}

// DeleteFromTask always returns ErrAppendOnlyViolation.
// The task_commits table is INSERT-only per FR-12.
// Refs: FR-12
func (s *Store) DeleteFromTask(_ context.Context, _, _ string) error {
	return fmt.Errorf("%w: task_commits is append-only", model.ErrAppendOnlyViolation)
}
