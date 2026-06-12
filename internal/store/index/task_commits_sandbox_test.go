// Package index tests verify sandbox_id provenance on task_commits per
// MGIT-11.3.3 acceptance criteria. Refs: FR-17.18, FR-17.6, NFR-3.1
package index

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// legacyTaskCommitsSQL recreates the pre-FR-17 task_commits shape
// (no sandbox_id) so the additive migration can be exercised.
const legacyTaskCommitsSQL = `
CREATE TABLE schema_version (version INTEGER NOT NULL, applied_at TEXT NOT NULL);
CREATE TABLE task_commits (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id TEXT NOT NULL,
    commit_hash TEXT NOT NULL,
    content_hash TEXT NOT NULL DEFAULT '',
    agent_id TEXT NOT NULL DEFAULT '',
    position INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL,
    UNIQUE(task_id, commit_hash)
);
INSERT INTO schema_version (version, applied_at) VALUES (1, '2026-04-07T12:00:00Z');
INSERT INTO task_commits (task_id, commit_hash, content_hash, agent_id, position, created_at)
VALUES ('MGIT-1.1', 'aaaa', 'cccc', 'agent-1', 0, '2026-04-07T12:00:00Z');
`

// TestTaskCommits_SandboxIDColumn_Additive verifies the migration adds
// the nullable sandbox_id column to a pre-existing database without
// touching existing rows. Refs: FR-17.18
func TestTaskCommits_SandboxIDColumn_Additive(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy.db")

	legacy, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	_, err = legacy.Exec(legacyTaskCommitsSQL)
	require.NoError(t, err)
	require.NoError(t, legacy.Close())

	store, err := New(dbPath, fixedClock())
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()

	var columns []string
	rows, err := store.readDB.QueryContext(ctx, "PRAGMA table_info(task_commits)")
	require.NoError(t, err)
	defer rows.Close() //nolint:errcheck // non-critical
	for rows.Next() {
		var cid, notNull, pk int
		var name, colType string
		var dflt any
		require.NoError(t, rows.Scan(&cid, &name, &colType, &notNull, &dflt, &pk))
		columns = append(columns, name)
	}
	require.NoError(t, rows.Err())
	assert.Contains(t, columns, "sandbox_id", "migration must add sandbox_id")

	// The pre-existing row survives untouched, with NULL sandbox_id.
	records, err := store.GetTaskCommits(ctx, "MGIT-1.1")
	require.NoError(t, err)
	require.Len(t, records, 1)
	assert.Equal(t, "aaaa", records[0].CommitHash)
	assert.Nil(t, records[0].SandboxID, "legacy rows must read back as unsandboxed")
}

// TestTaskCommits_NullSandboxID_QueryableAsUnsandboxed verifies the
// provenance contract: landed commits carry their sandbox; unsandboxed
// commits carry NULL, permanently visible and queryable (F-02, SEC-02).
// Refs: FR-17.6, FR-17.18
func TestTaskCommits_NullSandboxID_QueryableAsUnsandboxed(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	require.NoError(t, store.AppendTaskCommit(ctx, TaskCommitInsert{
		TaskID: "MGIT-4.2", CommitHash: "sandboxedhash", ContentHash: "c1",
		AgentID: "agent-1", Position: 0,
		SandboxID: "01JXSANDBOX00000000000000",
	}))
	require.NoError(t, store.AppendTaskCommit(ctx, TaskCommitInsert{
		TaskID: "MGIT-4.2", CommitHash: "hosthash", ContentHash: "c2",
		AgentID: "agent-1", Position: 1,
	}))

	records, err := store.GetTaskCommits(ctx, "MGIT-4.2")
	require.NoError(t, err)
	require.Len(t, records, 2)
	require.NotNil(t, records[0].SandboxID)
	assert.Equal(t, "01JXSANDBOX00000000000000", *records[0].SandboxID)
	assert.Nil(t, records[1].SandboxID)

	unsandboxed, err := store.GetUnsandboxedCommits(ctx)
	require.NoError(t, err)
	require.Len(t, unsandboxed, 1, "exactly the NULL-sandbox row is unsandboxed")
	assert.Equal(t, "hosthash", unsandboxed[0].CommitHash)
}

// TestTaskCommits_StillAppendOnly verifies the append-only laws
// survived the column addition: no UPDATE/DELETE path in code, the
// delete API still refuses, and legacy insert paths still work.
// Refs: NFR-3.1, FR-12
func TestTaskCommits_StillAppendOnly(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	err := store.DeleteFromTask(ctx, "MGIT-4.2", "anyhash")
	assert.ErrorIs(t, err, model.ErrAppendOnlyViolation)

	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	mutationRe := regexp.MustCompile(`(?i)\b(update\s+task_commits|delete\s+from\s+task_commits)\b`)
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Clean(entry.Name()))
		require.NoError(t, err)
		assert.False(t, mutationRe.Match(src),
			"%s must not contain UPDATE/DELETE touching task_commits", entry.Name())
	}

	t.Run("legacy_add_path_inserts_null_sandbox", func(t *testing.T) {
		require.NoError(t, store.AddCommitToTask(ctx, "MGIT-5.1", "legacypath", "c3", "agent-1", 0))
		records, err := store.GetTaskCommits(ctx, "MGIT-5.1")
		require.NoError(t, err)
		require.Len(t, records, 1)
		assert.Nil(t, records[0].SandboxID)
	})

	t.Run("duplicate_still_rejected", func(t *testing.T) {
		err := store.AddCommitToTask(ctx, "MGIT-5.1", "legacypath", "c3", "agent-1", 1)
		assert.Error(t, err, "UNIQUE(task_id, commit_hash) must survive the migration")
	})
}
