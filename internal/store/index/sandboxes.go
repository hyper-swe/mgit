package index

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/hyper-swe/mgit/internal/model"
)

// The durable sandbox registry (MGIT-102).
//
// WHY THIS TABLE EXISTS. The live registry used to be daemon-process memory
// alone, on the reasoning that a microVM is a child of the daemon so a
// restart takes its VMs with it and a fresh daemon "correctly starts empty".
// That reasoning does not hold for the state a registration is USUALLY in:
// provisioning is lazy (FR-17.9, FR-17.10), so `mgit work --sandbox` registers
// WITHOUT booting, a never-used sandbox holds no VM keeping anything alive,
// and its daemon idle-exits (NFR-17.6) 30 seconds later. The registration went
// with it, `mgit sandbox status` answered "sandbox not found", and an agent
// that had been told it was contained found `mgit run` refusing.
//
// WHY IT IS SEPARATE FROM sandbox_events. This is live state (mutable, one row
// per sandbox, deleted at teardown). sandbox_events is the append-only history
// and stays the audit trail: every terminal transition — including a sandbox
// discarded at rehydration because its VM could no longer be verified — is
// recorded there. Neither table substitutes for the other, and neither ever
// touches task_commits.
// Refs: FR-17.1, FR-17.9, FR-17.10, FR-17.18, MGIT-102

// UpsertSandbox writes one sandbox registration, replacing any prior row for
// the same sandbox ID. The registration is validated first: a row here is a
// claim that a sandbox EXISTS, so a hollow one would resurrect a sandbox that
// cannot be launched. The UNIQUE(task_id) / UNIQUE(worktree_path) constraints
// surface an exclusivity violation (FR-17.1) as an error rather than letting a
// second sandbox claim a bound task.
// Refs: FR-17.1, FR-17.10, MGIT-102
func (s *Store) UpsertSandbox(ctx context.Context, reg *model.SandboxRegistration) error {
	if reg == nil {
		return fmt.Errorf("upsert sandbox: registration must not be nil")
	}
	if err := reg.Validate(); err != nil {
		return fmt.Errorf("upsert sandbox: %w", err)
	}
	allowlist, ports, err := encodeSandboxLists(reg.Info)
	if err != nil {
		return err
	}

	// One row per sandbox: re-registering the same ID replaces the live view.
	// task_id / worktree_path stay UNIQUE, so a conflicting claim errors here.
	const upsertSQL = `INSERT INTO sandboxes
		(sandbox_id, task_id, worktree_path, image_ref, image_digest, backend,
		 network_mode, network_allowlist, publish_ports, cpus, memory_mb,
		 disk_quota_mb, ttl_ns, confine_agent, state, created_at, expires_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(sandbox_id) DO UPDATE SET
			task_id = excluded.task_id,
			worktree_path = excluded.worktree_path,
			image_ref = excluded.image_ref,
			image_digest = excluded.image_digest,
			backend = excluded.backend,
			network_mode = excluded.network_mode,
			network_allowlist = excluded.network_allowlist,
			publish_ports = excluded.publish_ports,
			cpus = excluded.cpus,
			memory_mb = excluded.memory_mb,
			disk_quota_mb = excluded.disk_quota_mb,
			ttl_ns = excluded.ttl_ns,
			confine_agent = excluded.confine_agent,
			state = excluded.state,
			expires_at = excluded.expires_at,
			updated_at = excluded.updated_at`

	now := s.clock().UTC().Format(time.RFC3339)
	return s.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, upsertSQL,
			reg.Info.ID, reg.Info.TaskID, reg.Info.WorktreePath, reg.ImageRef,
			reg.Info.ImageDigest, reg.Info.Backend, reg.Info.NetworkMode,
			allowlist, ports, reg.Info.CPUs, reg.Info.MemoryMB, reg.Info.DiskQuotaMB,
			int64(reg.TTL), boolToInt(reg.ConfineAgent), reg.Info.State,
			formatRegistryTime(reg.Info.CreatedAt), formatRegistryTime(reg.Info.ExpiresAt), now)
		if err != nil {
			return fmt.Errorf("upsert sandbox %s: %w", reg.Info.ID, err)
		}
		return nil
	})
}

// SetSandboxState records the state a running daemon last OBSERVED, so the
// next daemon reconciles against that rather than against the registration-time
// state. An unknown sandbox is an error, never a silent no-op: it means the
// caller and the registry disagree about what exists.
// Refs: FR-17.18, MGIT-102
func (s *Store) SetSandboxState(ctx context.Context, sandboxID, state string) error {
	if !model.ValidSandboxState(state) {
		return fmt.Errorf("set sandbox state: unknown state %q", state)
	}
	// Live-state update on the registry (NOT an audit table); the transition
	// itself is recorded append-only in sandbox_events by the service.
	const updateSQL = `UPDATE sandboxes SET state = ?, updated_at = ? WHERE sandbox_id = ?`

	return s.WriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, updateSQL, state,
			s.clock().UTC().Format(time.RFC3339), sandboxID)
		if err != nil {
			return fmt.Errorf("set sandbox state: %w", err)
		}
		return requireOneRow(res, sandboxID)
	})
}

// DeleteSandbox removes a sandbox from the live registry at teardown, freeing
// its task and worktree bindings. The sandbox's history is untouched: the
// terminal event stays in sandbox_events, which is where an auditor reads what
// happened. Refs: FR-17.9, FR-17.18, MGIT-102
func (s *Store) DeleteSandbox(ctx context.Context, sandboxID string) error {
	return s.WriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, "DELETE FROM sandboxes WHERE sandbox_id = ?", sandboxID)
		if err != nil {
			return fmt.Errorf("delete sandbox: %w", err)
		}
		return requireOneRow(res, sandboxID)
	})
}

// ListSandboxes returns every registration in the durable registry, ordered by
// task ID so daemon-start rehydration is deterministic. Refs: MGIT-102
func (s *Store) ListSandboxes(ctx context.Context) ([]model.SandboxRegistration, error) {
	// Full live roster, task-ordered (the rehydration read at daemon start)
	const querySQL = `SELECT sandbox_id, task_id, worktree_path, image_ref, image_digest,
		backend, network_mode, network_allowlist, publish_ports, cpus, memory_mb,
		disk_quota_mb, ttl_ns, confine_agent, state, created_at, expires_at
		FROM sandboxes ORDER BY task_id`

	var out []model.SandboxRegistration
	err := s.ReadTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, querySQL)
		if err != nil {
			return err
		}
		defer rows.Close() //nolint:errcheck // non-critical

		for rows.Next() {
			reg, scanErr := scanSandboxRow(rows)
			if scanErr != nil {
				return scanErr
			}
			out = append(out, reg)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("list sandboxes: %w", err)
	}
	return out, nil
}

// scanSandboxRow decodes one registry row, including the two list-valued
// columns held as JSON. A malformed row is reported as index corruption rather
// than silently rehydrated as a half-configured sandbox — a sandbox brought
// back with the wrong egress posture would be worse than one not brought back.
// Refs: MGIT-102, SEC-04
func scanSandboxRow(rows *sql.Rows) (model.SandboxRegistration, error) {
	var reg model.SandboxRegistration
	var allowlist, ports, createdAt, expiresAt string
	var ttlNS int64
	var confine int
	if err := rows.Scan(&reg.Info.ID, &reg.Info.TaskID, &reg.Info.WorktreePath,
		&reg.ImageRef, &reg.Info.ImageDigest, &reg.Info.Backend, &reg.Info.NetworkMode,
		&allowlist, &ports, &reg.Info.CPUs, &reg.Info.MemoryMB, &reg.Info.DiskQuotaMB,
		&ttlNS, &confine, &reg.Info.State, &createdAt, &expiresAt); err != nil {
		return reg, err
	}
	reg.TTL = time.Duration(ttlNS)
	reg.ConfineAgent = confine != 0
	if err := decodeJSONColumn(allowlist, &reg.Info.NetworkAllowlist); err != nil {
		return reg, fmt.Errorf("%w: sandbox %s network_allowlist: %w", model.ErrIndexCorrupted, reg.Info.ID, err)
	}
	if err := decodeJSONColumn(ports, &reg.Info.PublishPorts); err != nil {
		return reg, fmt.Errorf("%w: sandbox %s publish_ports: %w", model.ErrIndexCorrupted, reg.Info.ID, err)
	}
	var err error
	if reg.Info.CreatedAt, err = parseRegistryTime(createdAt); err != nil {
		return reg, fmt.Errorf("%w: sandbox %s created_at: %w", model.ErrIndexCorrupted, reg.Info.ID, err)
	}
	if reg.Info.ExpiresAt, err = parseRegistryTime(expiresAt); err != nil {
		return reg, fmt.Errorf("%w: sandbox %s expires_at: %w", model.ErrIndexCorrupted, reg.Info.ID, err)
	}
	return reg, nil
}

// encodeSandboxLists renders the two list-valued fields as JSON columns. They
// are the only non-scalar registration state; everything else gets its own
// column so the registry stays readable with plain SQL.
func encodeSandboxLists(info model.SandboxInfo) (allowlist, ports string, err error) {
	a, err := json.Marshal(info.NetworkAllowlist)
	if err != nil {
		return "", "", fmt.Errorf("encode network allowlist: %w", err)
	}
	p, err := json.Marshal(info.PublishPorts)
	if err != nil {
		return "", "", fmt.Errorf("encode publish ports: %w", err)
	}
	return string(a), string(p), nil
}

// decodeJSONColumn decodes a JSON list column, treating an empty column
// (a row written before the column existed) as an empty list.
func decodeJSONColumn(raw string, out any) error {
	if raw == "" || raw == "null" {
		return nil
	}
	return json.Unmarshal([]byte(raw), out)
}

// formatRegistryTime renders a timestamp for the registry, mapping the zero
// time to the empty string ("no deadline") rather than to year 1.
func formatRegistryTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// parseRegistryTime is formatRegistryTime's inverse; an empty column is the
// zero time, and anything else must parse — a malformed timestamp is
// corruption, never quietly rendered as year 1.
func parseRegistryTime(raw string) (time.Time, error) {
	if raw == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339, raw)
}

// requireOneRow turns a no-op UPDATE/DELETE into ErrSandboxNotFound so a write
// against a sandbox the registry does not hold is reported, not swallowed.
func requireOneRow(res sql.Result, sandboxID string) error {
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if affected == 0 {
		return fmt.Errorf("%w: sandbox %q", model.ErrSandboxNotFound, sandboxID)
	}
	return nil
}

// boolToInt stores a bool as SQLite's 0/1 integer.
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
