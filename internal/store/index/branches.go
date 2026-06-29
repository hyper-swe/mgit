package index

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/hyper-swe/mgit/internal/model"
)

// CreateBranch inserts a new branch record.
// Returns ErrBranchAlreadyExists if the name is taken.
// Refs: FR-5, MGIT-2.3.4
func (s *Store) CreateBranch(ctx context.Context, branch *model.Branch) error {
	const insertSQL = `INSERT INTO branches (name, task_id, head_commit, status, created_at)
		VALUES (?, ?, ?, 'active', ?)`

	return s.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, insertSQL,
			branch.Name, branch.TaskID.String(), branch.HeadCommit,
			s.clock().UTC().Format(time.RFC3339))
		if err != nil {
			return fmt.Errorf("%w: %s", model.ErrBranchAlreadyExists, branch.Name)
		}
		return nil
	})
}

// DeleteBranch removes a branch row from the index. It is idempotent: deleting
// a name with no row is NOT an error, so a stale row left by a ref-only delete
// can be cleared even when the ref is already gone (recovery). The `branches`
// table is index/cache state (task lookup, locks, squash metadata), NOT the
// append-only audit table (`task_commits`), so deleting a row here is allowed.
// Refs: MGIT-42, FR-5
func (s *Store) DeleteBranch(ctx context.Context, name string) error {
	return s.WriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "DELETE FROM branches WHERE name = ?", name); err != nil {
			return fmt.Errorf("delete branch: %w", err)
		}
		return nil
	})
}

// GetBranch retrieves a branch by name.
// Returns ErrBranchNotFound if the branch does not exist.
// Refs: FR-5
func (s *Store) GetBranch(ctx context.Context, name string) (*model.Branch, error) {
	const querySQL = `SELECT name, task_id, head_commit, locked_by, locked_until, is_merged, status, created_at
		FROM branches WHERE name = ?`

	var b model.Branch
	var taskIDStr, lockedUntilStr, createdAtStr string
	var isMerged int

	err := s.ReadTx(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, querySQL, name).Scan(
			&b.Name, &taskIDStr, &b.HeadCommit, &b.LockedBy,
			&lockedUntilStr, &isMerged, &b.BranchID, &createdAtStr)
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %s", model.ErrBranchNotFound, name)
	}

	b.IsMerged = isMerged != 0

	if taskIDStr != "" {
		tid, parseErr := model.ParseTaskID(taskIDStr)
		if parseErr == nil {
			b.TaskID = tid
		}
	}
	if lockedUntilStr != "" {
		t, parseErr := time.Parse(time.RFC3339, lockedUntilStr)
		if parseErr == nil {
			b.LockedUntil = t
		}
	}
	if createdAtStr != "" {
		t, parseErr := time.Parse(time.RFC3339, createdAtStr)
		if parseErr == nil {
			b.CreatedAt = t
		}
	}

	return &b, nil
}

// UpdateBranchHead updates the head commit for a branch.
// Refs: FR-5
func (s *Store) UpdateBranchHead(ctx context.Context, name, commitHash string) error {
	const updateSQL = `UPDATE branches SET head_commit = ? WHERE name = ?`

	return s.WriteTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, updateSQL, commitHash, name)
		if err != nil {
			return fmt.Errorf("update branch head: %w", err)
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("check rows affected: %w", err)
		}
		if rows == 0 {
			return fmt.Errorf("%w: %s", model.ErrBranchNotFound, name)
		}
		return nil
	})
}

// LockBranch acquires an advisory lock on a branch.
// Returns ErrBranchLocked if already locked by another agent.
// Refs: FR-5, NFR-3.5
func (s *Store) LockBranch(ctx context.Context, name, agentID string, duration time.Duration) error {
	now := s.clock().UTC()
	expiresAt := now.Add(duration)

	return s.WriteTx(ctx, func(tx *sql.Tx) error {
		// Check existing lock
		var existingAgent, existingExpires string
		err := tx.QueryRowContext(ctx,
			"SELECT agent_id, expires_at FROM branch_locks WHERE branch_name = ?", name,
		).Scan(&existingAgent, &existingExpires)

		if err == nil {
			// Lock exists — check if expired
			expiry, parseErr := time.Parse(time.RFC3339, existingExpires)
			if parseErr == nil && now.Before(expiry) && existingAgent != agentID {
				return fmt.Errorf("%w: held by %s until %s",
					model.ErrBranchLocked, existingAgent, existingExpires)
			}
			// Lock expired or same agent — update it
			_, err = tx.ExecContext(ctx,
				"UPDATE branch_locks SET agent_id = ?, operation = 'lock', locked_at = ?, expires_at = ? WHERE branch_name = ?",
				agentID, now.Format(time.RFC3339), expiresAt.Format(time.RFC3339), name)
			return err
		}

		// No existing lock — insert
		_, err = tx.ExecContext(ctx,
			"INSERT INTO branch_locks (branch_name, agent_id, operation, locked_at, expires_at) VALUES (?, ?, 'lock', ?, ?)",
			name, agentID, now.Format(time.RFC3339), expiresAt.Format(time.RFC3339))
		return err
	})
}

// UnlockBranch releases the advisory lock on a branch.
// Refs: FR-5
func (s *Store) UnlockBranch(ctx context.Context, name string) error {
	return s.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			"DELETE FROM branch_locks WHERE branch_name = ?", name)
		return err
	})
}

// ListBranches returns all branches.
// Refs: FR-5
func (s *Store) ListBranches(ctx context.Context) ([]*model.Branch, error) {
	const querySQL = `SELECT name, task_id, head_commit, is_merged, status, created_at FROM branches`

	var branches []*model.Branch
	err := s.ReadTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, querySQL)
		if err != nil {
			return err
		}
		defer rows.Close() //nolint:errcheck // non-critical after read

		for rows.Next() {
			var b model.Branch
			var taskIDStr, status, createdAtStr string
			var isMerged int
			if err := rows.Scan(&b.Name, &taskIDStr, &b.HeadCommit, &isMerged, &status, &createdAtStr); err != nil {
				return err
			}
			b.IsMerged = isMerged != 0
			b.BranchID = status
			if taskIDStr != "" {
				tid, parseErr := model.ParseTaskID(taskIDStr)
				if parseErr == nil {
					b.TaskID = tid
				}
			}
			if createdAtStr != "" {
				t, parseErr := time.Parse(time.RFC3339, createdAtStr)
				if parseErr == nil {
					b.CreatedAt = t
				}
			}
			branches = append(branches, &b)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return branches, nil
}
