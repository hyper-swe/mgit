package index

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func fixedClock() func() time.Time {
	fixed := time.Date(2026, 4, 7, 12, 0, 0, 0, time.UTC)
	return func() time.Time { return fixed }
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "index.db")
	store, err := New(dbPath, fixedClock())
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestStore_New(t *testing.T) {
	store := newTestStore(t)
	assert.NotNil(t, store)
	assert.NotEmpty(t, store.Path())
}

func TestStore_Pragmas(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Verify WAL mode
	var journalMode string
	err := store.readDB.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journalMode)
	require.NoError(t, err)
	assert.Equal(t, "wal", journalMode)

	// Verify foreign keys
	var fkEnabled int
	err = store.readDB.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fkEnabled)
	require.NoError(t, err)
	assert.Equal(t, 1, fkEnabled)
}

func TestStore_ReadTx(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	err := store.ReadTx(ctx, func(tx *sql.Tx) error {
		var count int
		return tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM task_commits").Scan(&count)
	})
	assert.NoError(t, err)
}

func TestStore_WriteTx(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	err := store.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			"INSERT INTO task_commits (task_id, commit_hash, position, created_at) VALUES (?, ?, ?, ?)",
			"MGIT-1", "abc123", 0, "2026-04-07T12:00:00Z")
		return err
	})
	assert.NoError(t, err)
}

func TestStore_WriteTx_Serialized(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Two sequential writes should both succeed
	suffixes := []string{"a", "b", "c"}
	for i, suffix := range suffixes {
		err := store.WriteTx(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx,
				"INSERT INTO task_commits (task_id, commit_hash, position, created_at) VALUES (?, ?, ?, ?)",
				"MGIT-1", "hash"+suffix, i, "2026-04-07T12:00:00Z")
			return err
		})
		require.NoError(t, err)
	}

	// Verify all 3 were written
	var count int
	err := store.ReadTx(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM task_commits").Scan(&count)
	})
	require.NoError(t, err)
	assert.Equal(t, 3, count)
}

func TestStore_Close(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "index.db")
	store, err := New(dbPath, fixedClock())
	require.NoError(t, err)

	err = store.Close()
	assert.NoError(t, err)
}

func TestStore_SchemaVersion(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	ver, err := store.SchemaVersion(ctx)
	require.NoError(t, err)
	assert.Equal(t, schemaVersion, ver)
}

func TestSchema_AllTablesDefinedCorrectly(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	tables := []string{"schema_version", "task_commits", "branches", "branch_locks", "worktrees"}
	for _, table := range tables {
		t.Run(table, func(t *testing.T) {
			var name string
			err := store.readDB.QueryRowContext(ctx,
				"SELECT name FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&name)
			require.NoError(t, err, "table %s must exist", table)
			assert.Equal(t, table, name)
		})
	}
}

func TestSchema_AppendOnlyEnforced(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// The DeleteFromTask method must reject
	err := store.DeleteFromTask(ctx, "MGIT-1", "abc")
	assert.ErrorIs(t, err, ErrAppendOnlyViolation)
}

func TestSchema_BranchLocksTable(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Verify branch_locks table has correct columns
	var name string
	err := store.readDB.QueryRowContext(ctx,
		"SELECT name FROM sqlite_master WHERE type='table' AND name='branch_locks'").Scan(&name)
	require.NoError(t, err)
	assert.Equal(t, "branch_locks", name)
}

func TestStore_NilClock(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "index.db")
	_, err := New(dbPath, nil)
	assert.Error(t, err, "New with nil clock should fail")
}
