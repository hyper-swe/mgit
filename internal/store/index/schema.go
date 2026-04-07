// Package index implements the SQLite task-commit index for mgit.
// This is the source of truth for which commits belong to which tasks.
// All task_commits operations are APPEND-ONLY per FR-12.
// Refs: FR-4, FR-5, FR-12, NFR-3
package index

// schemaVersion tracks the current schema version for migrations.
const schemaVersion = 1

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
    UNIQUE(branch_name),
    UNIQUE(task_id)
);
`
