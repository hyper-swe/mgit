package index

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Content origin and delivery are host decisions, independent of sandbox policy.
// Missing events are ambiguous, not evidence that a legacy row was host-authored.
const (
	OriginHost         = "host"
	OriginLand         = "land"
	AdmissionPending   = "pending"
	AdmissionDelivered = "delivered"
)

func insertAdmission(ctx context.Context, tx *sql.Tx, r CommitRecord, origin, state, guestHash, now string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO content_admissions
 (task_commit_id, task_id, commit_hash, content_hash, origin, state, guest_content_hash, created_at)
 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, r.ID, r.TaskID, r.CommitHash, r.ContentHash, origin, state, guestHash, now)
	return err
}

// ContentRecord is a single consistent snapshot of a ledger row and its latest
// locally recorded admission. It is not populated from transported assertions.
type ContentRecord struct {
	CommitRecord
	Origin           string
	State            string
	GuestContentHash string
}

const contentSnapshotSQL = `SELECT t.id, t.task_id, t.commit_hash, t.content_hash,
 t.agent_id, t.position, t.created_at, t.sandbox_id,
 COALESCE(a.origin, ''), COALESCE(a.state, ''), COALESCE(a.guest_content_hash, '')
 FROM task_commits t LEFT JOIN content_admissions a
 ON a.id = (SELECT MAX(last.id) FROM content_admissions last
 WHERE last.task_commit_id = t.id AND last.task_id = t.task_id
 AND last.commit_hash = t.commit_hash AND last.content_hash = t.content_hash)`

func readContentSnapshot(ctx context.Context, tx *sql.Tx, where string, arg string) ([]ContentRecord, error) {
	rows, err := tx.QueryContext(ctx, contentSnapshotSQL+where, arg)
	if err != nil {
		return nil, fmt.Errorf("read content eligibility: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var result []ContentRecord
	for rows.Next() {
		var r ContentRecord
		var sandbox sql.NullString
		if err := rows.Scan(&r.ID, &r.TaskID, &r.CommitHash, &r.ContentHash, &r.AgentID, &r.Position, &r.CreatedAt, &sandbox, &r.Origin, &r.State, &r.GuestContentHash); err != nil {
			return nil, err
		}
		if sandbox.Valid {
			r.SandboxID = &sandbox.String
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

// GetContentSnapshot reads ledger and admissions in ONE read transaction. Raw
// getters remain unfiltered for audit, duplicate checks and position allocation.
func (s *Store) GetContentSnapshot(ctx context.Context, task string) ([]ContentRecord, error) {
	var result []ContentRecord
	err := s.ReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		result, err = readContentSnapshot(ctx, tx, " WHERE t.task_id = ? ORDER BY t.position, t.id", task)
		return err
	})
	return result, err
}

// ContentRecoveryHint preserves the old audit store and avoids the task binding
// of linked worktrees. Initializing BEFORE copying ensures inspected files are
// new task content, rather than silently becoming the new repository's base.
const ContentRecoveryHint = "preserve the original repository for audit; create a new empty directory outside it, run `mgit init` there BEFORE copying, copy only inspected source files (not .mgit or .git) into it, then run `mgit commit -a --task-id RECOVERY-1 -m 'Recover inspected files'` there, not in the old task-bound worktree"

func ambiguousContent(r CommitRecord) error {
	return fmt.Errorf("ambiguous content eligibility: task %s row %d commit %s; %s", r.TaskID, r.ID, r.CommitHash, ContentRecoveryHint)
}

// RequireKnownContentOrigins refuses origin inference for legacy/imported rows.
// Projection recovery shares this rule with emitters; a raw ledger row alone
// cannot authorize even an attempted delivery. Pending land rows are known,
// but remain ineligible for export until the final delivery event.
func RequireKnownContentOrigins(records []ContentRecord) error {
	for _, r := range records {
		known := r.Origin == OriginHost && r.State == AdmissionDelivered || r.Origin == OriginLand && (r.State == AdmissionPending || r.State == AdmissionDelivered)
		if !known {
			return ambiguousContent(r.CommitRecord)
		}
	}
	return nil
}

// GetContentCommits is the common content boundary. Pending imports remain in
// audit but are excluded; unknown legacy/imported origin is a loud error.
func (s *Store) GetContentCommits(ctx context.Context, task string) ([]CommitRecord, error) {
	snapshot, err := s.GetContentSnapshot(ctx, task)
	if err != nil {
		return nil, err
	}
	if err := RequireKnownContentOrigins(snapshot); err != nil {
		return nil, err
	}
	var result []CommitRecord
	for _, r := range snapshot {
		if r.State == AdmissionDelivered {
			result = append(result, r.CommitRecord)
		}
	}
	if len(snapshot) > 0 && len(result) == 0 {
		return nil, fmt.Errorf("task %s: no delivered content (retained commits are undelivered); %s", task, ContentRecoveryHint)
	}
	return result, nil
}

// UnindexedContentCommitError distinguishes a commit with no task ledger row
// from a legacy/imported row whose origin is ambiguous. It does not grant
// permission to use that commit as content.
type UnindexedContentCommitError struct {
	Hash string
}

func (e *UnindexedContentCommitError) Error() string {
	return fmt.Sprintf("commit %s is not an admitted task commit (no local task row); select a locally admitted task commit hash; initial and sync-base snapshots cannot be used as content sources", e.Hash)
}

// RequireContentCommit checks a resolved SOURCE hash before content mutation.
// Unknown hashes cannot bypass the local ledger boundary.
func (s *Store) RequireContentCommit(ctx context.Context, hash string) error {
	return s.ReadTx(ctx, func(tx *sql.Tx) error {
		rows, err := readContentSnapshot(ctx, tx, " WHERE t.commit_hash = ? ORDER BY t.id", hash)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return &UnindexedContentCommitError{Hash: hash}
		}
		if err := RequireKnownContentOrigins(rows); err != nil {
			return err
		}
		for _, r := range rows {
			if r.State != AdmissionDelivered {
				return fmt.Errorf("undelivered commit %s (task %s row %d); %s", hash, r.TaskID, r.ID, ContentRecoveryHint)
			}
		}
		return nil
	})
}

// RecordDelivered appends only for existing, host-bound pending rows. Call ONLY
// after trusted projection and read-back under the repository operation lock.
// It cannot reconcile unknowable legacy origin or bless a foreign bundle.
func (s *Store) RecordDelivered(ctx context.Context, records []CommitRecord) error {
	return s.WriteTx(ctx, func(tx *sql.Tx) error {
		now := s.clock().UTC().Format(time.RFC3339)
		for _, rec := range records {
			rows, err := readContentSnapshot(ctx, tx, " WHERE t.commit_hash = ? ORDER BY t.id", rec.CommitHash)
			if err != nil {
				return err
			}
			var found *ContentRecord
			for i := range rows {
				if rows[i].ID == rec.ID && rows[i].TaskID == rec.TaskID && rows[i].ContentHash == rec.ContentHash {
					found = &rows[i]
					break
				}
			}
			if found == nil {
				return fmt.Errorf("delivery row binding mismatch: task %s row %d commit %s", rec.TaskID, rec.ID, rec.CommitHash)
			}
			if err := RequireKnownContentOrigins([]ContentRecord{*found}); err != nil {
				return err
			}
			if found.State == AdmissionDelivered {
				continue
			}
			if found.Origin != OriginLand || found.State != AdmissionPending {
				return fmt.Errorf("invalid admission transition for row %d", rec.ID)
			}
			if err := insertAdmission(ctx, tx, rec, found.Origin, AdmissionDelivered, found.GuestContentHash, now); err != nil {
				return err
			}
		}
		return nil
	})
}
