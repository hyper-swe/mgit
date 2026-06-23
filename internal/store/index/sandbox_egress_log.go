package index

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/hyper-swe/mgit/internal/model"
)

// maxEgressHostLen caps the guest-influenced dest_host field; DNS names max out
// at 253 bytes, so anything longer is hostile padding (SEC-07 label-encoding
// exfiltration also motivates the cap). Single-sourced from the model boundary
// cap so the two can never drift (the model rejects oversized hosts; this
// truncates whatever survives). Refs: F-09
const maxEgressHostLen = model.MaxEgressDestHostLen

// AppendEgressRecord appends one proxy allow/deny decision to the
// append-only sandbox_egress_log. The store assigns the ULID id and
// created_at; guest-influenced strings are sanitized and length-capped
// first. No update or delete path exists by construction (F-01 laws).
// Refs: FR-17.8, FR-17.18, MGIT-11.3.5
func (s *Store) AppendEgressRecord(ctx context.Context, rec *model.EgressRecord) error {
	if err := rec.Validate(); err != nil {
		return fmt.Errorf("append egress record: %w", err)
	}

	id, err := s.newULID()
	if err != nil {
		return fmt.Errorf("append egress record: %w", err)
	}

	// Insert one immutable audit row (APPEND-ONLY: never UPDATE/DELETE)
	const insertSQL = `INSERT INTO sandbox_egress_log
		(id, sandbox_id, task_id, decision, protocol, dest_host, dest_ip, dest_port, rule, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	return s.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, insertSQL,
			id, rec.SandboxID, rec.TaskID, rec.Decision, rec.Protocol,
			sanitizeAuditString(rec.DestHost, maxEgressHostLen),
			rec.DestIP, rec.DestPort,
			sanitizeAuditString(rec.Rule, maxSandboxEventDetailLen),
			s.clock().UTC().Format(time.RFC3339))
		if err != nil {
			return fmt.Errorf("insert egress record: %w", err)
		}
		return nil
	})
}

// ListEgressBySandbox returns one sandbox's egress decisions in append
// order. Refs: FR-17.18
func (s *Store) ListEgressBySandbox(ctx context.Context, sandboxID string) ([]model.EgressRecord, error) {
	// Per-sandbox audit stream (indexed)
	const querySQL = `SELECT id, sandbox_id, task_id, decision, protocol,
		dest_host, dest_ip, dest_port, rule, created_at
		FROM sandbox_egress_log WHERE sandbox_id = ? ORDER BY id`
	return s.queryEgressRecords(ctx, querySQL, sandboxID)
}

// ListEgressByTask returns a task's egress decisions across all of its
// sandboxes in append order. Refs: FR-17.18
func (s *Store) ListEgressByTask(ctx context.Context, taskID string) ([]model.EgressRecord, error) {
	// Per-task audit stream (indexed)
	const querySQL = `SELECT id, sandbox_id, task_id, decision, protocol,
		dest_host, dest_ip, dest_port, rule, created_at
		FROM sandbox_egress_log WHERE task_id = ? ORDER BY id`
	return s.queryEgressRecords(ctx, querySQL, taskID)
}

// queryEgressRecords runs one parameterized egress-log SELECT whose
// column list matches EgressRecord.
func (s *Store) queryEgressRecords(ctx context.Context, querySQL string, args ...any) ([]model.EgressRecord, error) {
	var records []model.EgressRecord
	err := s.ReadTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, querySQL, args...)
		if err != nil {
			return err
		}
		defer rows.Close() //nolint:errcheck // non-critical

		for rows.Next() {
			var rec model.EgressRecord
			var createdAt string
			if err := rows.Scan(&rec.ID, &rec.SandboxID, &rec.TaskID, &rec.Decision,
				&rec.Protocol, &rec.DestHost, &rec.DestIP, &rec.DestPort,
				&rec.Rule, &createdAt); err != nil {
				return err
			}
			t, parseErr := time.Parse(time.RFC3339, createdAt)
			if parseErr != nil {
				// A malformed timestamp in an audit row is integrity
				// corruption, never quietly rendered as year 1.
				return fmt.Errorf("%w: bad created_at %q", model.ErrIndexCorrupted, createdAt)
			}
			rec.CreatedAt = t
			records = append(records, rec)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("query egress records: %w", err)
	}
	return records, nil
}
