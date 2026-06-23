package index

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/hyper-swe/mgit/internal/model"
)

// maxSandboxEventDetailLen caps the detail payload of one sandbox
// event. Guest-influenced strings land in an append-only table where
// corrupted or oversized entries are permanent (F-09).
const maxSandboxEventDetailLen = 4096

// AppendSandboxEvent appends one lifecycle event to the append-only
// sandbox_events table. The store assigns the ULID id and created_at;
// the detail payload is sanitized and length-capped before insertion.
// There is deliberately no update or delete counterpart (F-01, NFR-3.1).
// Refs: FR-17.18, MGIT-11.3.1
func (s *Store) AppendSandboxEvent(ctx context.Context, ev *model.SandboxEvent) error {
	if err := ev.Validate(); err != nil {
		return fmt.Errorf("append sandbox event: %w", err)
	}

	id, err := s.newULID()
	if err != nil {
		return fmt.Errorf("append sandbox event: %w", err)
	}

	// Insert one immutable audit row (APPEND-ONLY: never UPDATE/DELETE)
	const insertSQL = `INSERT INTO sandbox_events
		(id, sandbox_id, task_id, event_type, backend, image_digest, network_mode, detail, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`

	return s.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, insertSQL,
			id, ev.SandboxID, ev.TaskID, ev.EventType,
			ev.Backend, ev.ImageDigest, ev.NetworkMode,
			sanitizeAuditDetail(ev.Detail),
			s.clock().UTC().Format(time.RFC3339))
		if err != nil {
			return fmt.Errorf("insert sandbox event: %w", err)
		}
		return nil
	})
}

// ListSandboxEvents returns the full event stream for one sandbox in
// append (ULID) order. An unknown sandbox yields an empty stream.
// Refs: FR-17.18
func (s *Store) ListSandboxEvents(ctx context.Context, sandboxID string) ([]model.SandboxEvent, error) {
	// Per-sandbox event stream in append order (O(log n) via index)
	const querySQL = `SELECT id, sandbox_id, task_id, event_type, backend,
		image_digest, network_mode, detail, created_at
		FROM sandbox_events WHERE sandbox_id = ? ORDER BY id`

	var events []model.SandboxEvent
	err := s.ReadTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, querySQL, sandboxID)
		if err != nil {
			return err
		}
		defer rows.Close() //nolint:errcheck // non-critical

		for rows.Next() {
			var ev model.SandboxEvent
			var createdAt string
			if err := rows.Scan(&ev.ID, &ev.SandboxID, &ev.TaskID, &ev.EventType,
				&ev.Backend, &ev.ImageDigest, &ev.NetworkMode, &ev.Detail, &createdAt); err != nil {
				return err
			}
			t, parseErr := time.Parse(time.RFC3339, createdAt)
			if parseErr != nil {
				// A malformed timestamp in an audit row is integrity
				// corruption, never quietly rendered as year 1.
				return fmt.Errorf("%w: bad created_at %q", model.ErrIndexCorrupted, createdAt)
			}
			ev.CreatedAt = t
			events = append(events, ev)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("list sandbox events: %w", err)
	}
	return events, nil
}

// DeriveState computes a sandbox's current lifecycle state from its
// latest state-bearing event — there is no mutable session row (F-01).
// policy_granted events are skipped: they are audit records, not
// transitions. Returns ErrSandboxNotFound when no state-bearing event
// exists for the id. Refs: FR-17.18, MGIT-11.3.2
func (s *Store) DeriveState(ctx context.Context, sandboxID string) (string, error) {
	// Latest STATE-BEARING event for the sandbox: audit-only events
	// (policy_granted, credentials_injected) carry no state change and must
	// be skipped, else a healthy sandbox whose latest event is audit-only is
	// mis-read as corrupt. The excluded set is single-sourced from the model
	// vocabulary, so a new audit-only event is excluded automatically.
	// (O(log n) via the sandbox_id index; ULID ids sort in append order.)
	nonState := model.NonStateEventTypes()
	// Build a "?, ?, ..." placeholder list (placeholders only — no data — so
	// the query stays fully parameterized; SQL Rule 1).
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(nonState)), ",")
	querySQL := `SELECT event_type FROM sandbox_events
		WHERE sandbox_id = ? AND event_type NOT IN (` + placeholders + `)
		ORDER BY id DESC LIMIT 1`

	args := make([]any, 0, 1+len(nonState))
	args = append(args, sandboxID)
	for _, t := range nonState {
		args = append(args, t)
	}

	var eventType string
	err := s.ReadTx(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, querySQL, args...).Scan(&eventType)
	})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", fmt.Errorf("%w: %q", model.ErrSandboxNotFound, sandboxID)
	case err != nil:
		return "", fmt.Errorf("derive state: %w", err)
	}

	state, ok := model.StateForEvent(eventType)
	if !ok {
		// Unreachable for rows written through AppendSandboxEvent
		// (closed vocabulary); guards against external corruption.
		return "", fmt.Errorf("%w: corrupt event type %q", model.ErrIndexCorrupted, eventType)
	}
	return state, nil
}

// sanitizeAuditDetail sanitizes one event-detail payload (F-09).
func sanitizeAuditDetail(detail string) string {
	return sanitizeAuditString(detail, maxSandboxEventDetailLen)
}

// sanitizeAuditString strips control and format characters and caps
// the byte length of a guest-influenced string before it enters an
// append-only audit table, where a corrupted entry would be permanent
// (F-09). Format characters (Cf: RTL overrides, zero-width spaces,
// invisible tag characters) are stripped along with Cc/Cs and the
// Unicode line/paragraph separators — all are display-spoofing
// carriers in audit output. Truncation never splits a rune.
func sanitizeAuditString(value string, maxLen int) string {
	cleaned := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Cs, unicode.Zl, unicode.Zp) {
			return -1
		}
		return r
	}, value)

	if len(cleaned) > maxLen {
		// Byte-truncate, then drop any split trailing rune.
		return strings.ToValidUTF8(cleaned[:maxLen], "")
	}
	return cleaned
}
