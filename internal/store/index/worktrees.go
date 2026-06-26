package index

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/hyper-swe/mgit/internal/model"
)

// InsertWorktree adds a worktree record to the index.
// UNIQUE constraints on task_id and path enforce isolation.
// Refs: FR-16.11, MGIT-8.1.2
func (s *Store) InsertWorktree(ctx context.Context, wt *model.WorktreeInfo) error {
	const insertSQL = `INSERT INTO worktrees (path, branch_name, task_id, agent_id, created_at, fork_base)
		VALUES (?, ?, ?, ?, ?, ?)`

	return s.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, insertSQL,
			wt.Path, wt.Branch, wt.TaskID, wt.AgentID,
			s.clock().UTC().Format(time.RFC3339), wt.ForkBase)
		if err != nil {
			return fmt.Errorf("insert worktree: %w", err)
		}
		return nil
	})
}

// GetWorktree retrieves a worktree by path.
// Returns ErrWorktreeNotFound if not registered.
// Refs: FR-16
func (s *Store) GetWorktree(ctx context.Context, path string) (*model.WorktreeInfo, error) {
	const querySQL = `SELECT path, branch_name, task_id, agent_id, created_at, fork_base
		FROM worktrees WHERE path = ?`

	var wt model.WorktreeInfo
	var createdAt string

	err := s.ReadTx(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, querySQL, path).Scan(
			&wt.Path, &wt.Branch, &wt.TaskID, &wt.AgentID, &createdAt, &wt.ForkBase)
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %s", model.ErrWorktreeNotFound, path)
	}

	if t, parseErr := time.Parse(time.RFC3339, createdAt); parseErr == nil {
		wt.CreatedAt = t
	}
	wt.Name = model.DeriveNameFromPath(wt.Path)
	return &wt, nil
}

// GetWorktreeByTask retrieves the worktree bound to a task ID. The worktrees
// table has UNIQUE(task_id), so at most one row matches. Returns
// ErrWorktreeNotFound when no worktree is bound to the task (e.g. a task
// committed directly on the host store without `mgit work`). It backs the
// ADR-008 §4 pinned-fork-base assertion in squash/diff. Refs: MGIT-35, FR-16
func (s *Store) GetWorktreeByTask(ctx context.Context, taskID string) (*model.WorktreeInfo, error) {
	const querySQL = `SELECT path, branch_name, task_id, agent_id, created_at, fork_base
		FROM worktrees WHERE task_id = ?`

	var wt model.WorktreeInfo
	var createdAt string
	err := s.ReadTx(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, querySQL, taskID).Scan(
			&wt.Path, &wt.Branch, &wt.TaskID, &wt.AgentID, &createdAt, &wt.ForkBase)
	})
	if err != nil {
		return nil, fmt.Errorf("%w: task %s", model.ErrWorktreeNotFound, taskID)
	}
	if t, parseErr := time.Parse(time.RFC3339, createdAt); parseErr == nil {
		wt.CreatedAt = t
	}
	wt.Name = model.DeriveNameFromPath(wt.Path)
	return &wt, nil
}

// ListWorktrees returns all registered worktrees.
// Refs: FR-16
func (s *Store) ListWorktrees(ctx context.Context) ([]model.WorktreeInfo, error) {
	const querySQL = `SELECT path, branch_name, task_id, agent_id, created_at, fork_base FROM worktrees`

	var worktrees []model.WorktreeInfo
	err := s.ReadTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, querySQL)
		if err != nil {
			return err
		}
		defer rows.Close() //nolint:errcheck // non-critical

		for rows.Next() {
			var wt model.WorktreeInfo
			var createdAt string
			if err := rows.Scan(&wt.Path, &wt.Branch, &wt.TaskID, &wt.AgentID, &createdAt, &wt.ForkBase); err != nil {
				return err
			}
			if t, parseErr := time.Parse(time.RFC3339, createdAt); parseErr == nil {
				wt.CreatedAt = t
			}
			wt.Name = model.DeriveNameFromPath(wt.Path)
			worktrees = append(worktrees, wt)
		}
		return rows.Err()
	})
	return worktrees, err
}

// DeleteWorktree removes a worktree record.
// Returns ErrWorktreeNotFound if not registered.
// Refs: FR-16
func (s *Store) DeleteWorktree(ctx context.Context, path string) error {
	return s.WriteTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, "DELETE FROM worktrees WHERE path = ?", path)
		if err != nil {
			return fmt.Errorf("delete worktree: %w", err)
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if rows == 0 {
			return fmt.Errorf("%w: %s", model.ErrWorktreeNotFound, path)
		}
		return nil
	})
}
