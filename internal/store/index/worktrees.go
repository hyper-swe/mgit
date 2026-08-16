package index

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/hyper-swe/mgit/internal/model"
)

// InsertWorktree adds a worktree record to the index.
//
// The UNIQUE constraints on path, task_id and branch_name are not bookkeeping:
// they are what ENFORCES the FR-16 exclusivity rules (one worktree per path,
// per task, per branch) between concurrent `mgit work` processes. The repo-wide
// process lock serializes the racers, but this insert is the single point at
// which a winner is decided, so a conflict is translated into the matching
// named refusal rather than surfacing a raw SQLite constraint string.
// Refs: FR-16.11, MGIT-8.1.2, MGIT-120
func (s *Store) InsertWorktree(ctx context.Context, wt *model.WorktreeInfo) error {
	const insertSQL = `INSERT INTO worktrees (path, branch_name, task_id, agent_id, created_at, fork_base)
		VALUES (?, ?, ?, ?, ?, ?)`

	err := s.WriteTx(ctx, func(tx *sql.Tx) error {
		_, execErr := tx.ExecContext(ctx, insertSQL,
			wt.Path, wt.Branch, wt.TaskID, wt.AgentID,
			s.clock().UTC().Format(time.RFC3339), wt.ForkBase)
		if execErr != nil {
			return fmt.Errorf("insert worktree: %w", execErr)
		}
		return nil
	})
	if err != nil {
		return s.classifyWorktreeConflict(ctx, err, wt)
	}
	return nil
}

// classifyWorktreeConflict maps a UNIQUE-constraint failure on the worktrees
// table to the sentinel that names WHICH exclusivity rule refused the insert,
// and points at the worktree that already holds the contested resource. A
// non-constraint error is returned unchanged. The holder lookup is best-effort:
// the refusal must still be clear when the registry cannot be re-read.
// Refs: FR-16, MGIT-120
func (s *Store) classifyWorktreeConflict(ctx context.Context, err error, wt *model.WorktreeInfo) error {
	msg := err.Error()
	if !strings.Contains(msg, "UNIQUE constraint failed") {
		return err
	}
	switch {
	case strings.Contains(msg, "worktrees.task_id"):
		return fmt.Errorf("%w: task %s%s", model.ErrTaskAlreadyBound, wt.TaskID,
			s.holderSuffix(ctx, "task_id", wt.TaskID))
	case strings.Contains(msg, "worktrees.branch_name"):
		return fmt.Errorf("%w: branch %s%s", model.ErrBranchInUse, wt.Branch,
			s.holderSuffix(ctx, "branch_name", wt.Branch))
	case strings.Contains(msg, "worktrees.path"):
		return fmt.Errorf("%w: %s", model.ErrWorktreeExists, wt.Path)
	}
	return err
}

// holderSuffix returns " (held by worktree <path>)" for the row already holding
// the contested column value, or "" when it cannot be read. column is an
// internal literal (never user input), so it is safe to interpolate; the VALUE
// is always parameterized. Refs: MGIT-120
func (s *Store) holderSuffix(ctx context.Context, column, value string) string {
	var path string
	//nolint:gosec // column is one of two internal literals; the value is parameterized
	query := "SELECT path FROM worktrees WHERE " + column + " = ?"
	err := s.ReadTx(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, query, value).Scan(&path)
	})
	if err != nil || path == "" {
		return ""
	}
	return fmt.Sprintf(" (held by worktree %s)", path)
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
