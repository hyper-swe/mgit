// Package index implements the SQLite task-commit index for mgit.
// This is the source of truth for which commits belong to which tasks.
// All task_commits operations are APPEND-ONLY per FR-12.
// Refs: FR-4, FR-5, FR-12, NFR-3
package index

// schemaVersion tracks the current schema version for migrations.
// Version 2: FR-17 sandbox tables (sandbox_events, sandbox_egress_log)
// and the additive task_commits.sandbox_id column.
// Version 3: ADR-008 — additive worktrees.fork_base column pinning the base
// commit each task forked from (MGIT-35).
// Version 4: MGIT-102 — the `sandboxes` durable registry, so a sandbox
// registration survives the daemon process that created it.
const schemaVersion = 4

// createTablesSQL defines all tables for the mgit index database.
// task_commits is APPEND-ONLY: no UPDATE, no DELETE. Ever.
// Refs: FR-4 (task-commit mapping), FR-5 (branches), FR-12 (audit)
const createTablesSQL = `
-- Schema version tracking
CREATE TABLE IF NOT EXISTS schema_version (
    version INTEGER NOT NULL,
    applied_at TEXT NOT NULL
);

-- Task-commit mapping (APPEND-ONLY: INSERT only, never UPDATE or DELETE)
-- This is the core audit table for tracing which commits belong to which tasks.
-- Refs: FR-4, FR-12
CREATE TABLE IF NOT EXISTS task_commits (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id TEXT NOT NULL,
    commit_hash TEXT NOT NULL,
    content_hash TEXT NOT NULL DEFAULT '',
    agent_id TEXT NOT NULL DEFAULT '',
    position INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL,
    sandbox_id TEXT,  -- nullable: NULL = unsandboxed, a permanently visible gap (FR-17.6, F-02)
    UNIQUE(task_id, commit_hash)
);

-- Index for reverse lookup: commit -> task
CREATE INDEX IF NOT EXISTS idx_task_commits_commit_hash ON task_commits(commit_hash);
-- Index for task lookup: task -> commits
CREATE INDEX IF NOT EXISTS idx_task_commits_task_id ON task_commits(task_id);
-- Index for agent lookup
CREATE INDEX IF NOT EXISTS idx_task_commits_agent_id ON task_commits(agent_id);
-- Index for time-range queries
CREATE INDEX IF NOT EXISTS idx_task_commits_created_at ON task_commits(created_at);

-- Branch metadata
-- Refs: FR-5
CREATE TABLE IF NOT EXISTS branches (
    name TEXT PRIMARY KEY,
    task_id TEXT NOT NULL DEFAULT '',
    head_commit TEXT NOT NULL DEFAULT '',
    locked_by TEXT NOT NULL DEFAULT '',
    locked_until TEXT NOT NULL DEFAULT '',
    is_merged INTEGER NOT NULL DEFAULT 0,
    squash_commit TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'active',
    created_at TEXT NOT NULL
);

-- Index for task -> branch lookup
CREATE INDEX IF NOT EXISTS idx_branches_task_id ON branches(task_id);

-- Branch advisory locks for concurrent squash/rollback prevention
-- Refs: NFR-3.5
CREATE TABLE IF NOT EXISTS branch_locks (
    branch_name TEXT PRIMARY KEY,
    agent_id TEXT NOT NULL,
    operation TEXT NOT NULL,
    locked_at TEXT NOT NULL,
    expires_at TEXT NOT NULL
);

-- Worktree registry for multi-agent parallel development
-- Refs: FR-16
CREATE TABLE IF NOT EXISTS worktrees (
    path TEXT PRIMARY KEY,
    branch_name TEXT NOT NULL,
    task_id TEXT NOT NULL,
    agent_id TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    -- fork_base pins the base commit this task forked from (ADR-008 §4); a
    -- later base resync never shifts it, so squash/diff stay correct (MGIT-35).
    fork_base TEXT NOT NULL DEFAULT '',
    UNIQUE(branch_name),
    UNIQUE(task_id)
);

-- Sandbox lifecycle audit (APPEND-ONLY, event-sourced per F-01:
-- state is derived from the latest event; no ended_at column exists
-- because populating one would require UPDATE on an audit table).
-- Refs: FR-17.18
CREATE TABLE IF NOT EXISTS sandbox_events (
    id            TEXT PRIMARY KEY,   -- ULID (sortable: event order)
    sandbox_id    TEXT NOT NULL,      -- ULID of the sandbox
    task_id       TEXT NOT NULL,
    event_type    TEXT NOT NULL,      -- created | suspended | resumed |
                                      -- policy_granted | landed | destroyed |
                                      -- ttl_expired | killed
    backend       TEXT NOT NULL DEFAULT '',      -- kvm | vzf | hyperv | container
    image_digest  TEXT NOT NULL DEFAULT '',      -- sha256 of rootfs image
    network_mode  TEXT NOT NULL DEFAULT '',      -- none | allowlist | open
    detail        TEXT NOT NULL DEFAULT '',      -- JSON; sanitized + length-capped (F-09)
    created_at    TEXT NOT NULL       -- ISO-8601 UTC
);

-- Index for per-sandbox event streams (state derivation, FR-17.18)
CREATE INDEX IF NOT EXISTS idx_sandbox_events_sandbox_id ON sandbox_events(sandbox_id);
-- Index for per-task audit queries
CREATE INDEX IF NOT EXISTS idx_sandbox_events_task_id ON sandbox_events(task_id);

-- Durable registry of LIVE sandbox registrations (MGIT-102).
--
-- This table is deliberately MUTABLE, and it is deliberately NOT the audit
-- trail. sandbox_events above is the append-only history of what happened;
-- this is the current roster of what EXISTS, so a daemon that starts after
-- the one which registered a sandbox can bring it back instead of reporting
-- "sandbox not found" for containment a user was told they had. One row per
-- sandbox: state is updated in place and the row is deleted at teardown, with
-- the terminal transition recorded in sandbox_events.
--
-- UNIQUE(task_id) and UNIQUE(worktree_path) put FR-17.1 exclusivity in the
-- schema rather than only in daemon memory.
-- Refs: FR-17.1, FR-17.9, FR-17.10, FR-17.18, MGIT-102
CREATE TABLE IF NOT EXISTS sandboxes (
    sandbox_id        TEXT PRIMARY KEY,   -- ULID, host-assigned at registration
    task_id           TEXT NOT NULL,
    worktree_path     TEXT NOT NULL,
    image_ref         TEXT NOT NULL,      -- digest-pinned (FR-17.17)
    image_digest      TEXT NOT NULL DEFAULT '',
    backend           TEXT NOT NULL DEFAULT '',
    network_mode      TEXT NOT NULL DEFAULT '',
    network_allowlist TEXT NOT NULL DEFAULT '',  -- JSON array (list-valued)
    publish_ports     TEXT NOT NULL DEFAULT '',  -- JSON array (list-valued, SEC-09)
    cpus              INTEGER NOT NULL DEFAULT 0,   -- resolved effective caps,
    memory_mb         INTEGER NOT NULL DEFAULT 0,   -- not the requested ones
    disk_quota_mb     INTEGER NOT NULL DEFAULT 0,
    ttl_ns            INTEGER NOT NULL DEFAULT 0,
    confine_agent     INTEGER NOT NULL DEFAULT 0,
    state             TEXT NOT NULL,      -- last state this daemon OBSERVED
    created_at        TEXT NOT NULL,      -- ISO-8601 UTC, registration time
    expires_at        TEXT NOT NULL DEFAULT '',  -- TTL deadline; '' = no TTL
    updated_at        TEXT NOT NULL,      -- ISO-8601 UTC, last state write
    UNIQUE(task_id),
    UNIQUE(worktree_path)
);

-- Index for the task lookup every sandbox verb starts from
CREATE INDEX IF NOT EXISTS idx_sandboxes_task_id ON sandboxes(task_id);

-- Egress decisions in allowlist mode (APPEND-ONLY, same laws as
-- sandbox_events: no UPDATE, no DELETE, no retention pruning, ever).
-- Guest-influenced strings are sanitized + capped before insert
-- (SEC-04, SEC-07, F-09). Refs: FR-17.8, FR-17.18
CREATE TABLE IF NOT EXISTS sandbox_egress_log (
    id          TEXT PRIMARY KEY,   -- ULID (sortable: decision order)
    sandbox_id  TEXT NOT NULL,
    task_id     TEXT NOT NULL,
    decision    TEXT NOT NULL,      -- allow | deny
    protocol    TEXT NOT NULL,      -- tcp | udp | dns
    dest_host   TEXT NOT NULL DEFAULT '',  -- guest-influenced; sanitized
    dest_ip     TEXT NOT NULL DEFAULT '',  -- host-resolved destination
    dest_port   INTEGER NOT NULL DEFAULT 0,
    rule        TEXT NOT NULL DEFAULT '',  -- matched entry or deny reason
    created_at  TEXT NOT NULL       -- ISO-8601 UTC
);

-- Index for per-sandbox egress audit
CREATE INDEX IF NOT EXISTS idx_sandbox_egress_sandbox_id ON sandbox_egress_log(sandbox_id);
-- Index for per-task egress audit
CREATE INDEX IF NOT EXISTS idx_sandbox_egress_task_id ON sandbox_egress_log(task_id);
`

// postMigrationIndexSQL creates indexes on columns added by additive
// migrations; it must run AFTER ensureColumn so legacy databases gain
// the column first. Refs: FR-17.18
const postMigrationIndexSQL = `
-- Index for sandbox provenance queries (FR-17.18)
CREATE INDEX IF NOT EXISTS idx_task_commits_sandbox_id ON task_commits(sandbox_id);
`
