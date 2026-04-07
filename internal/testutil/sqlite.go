package testutil

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite" // Pure Go SQLite driver

	"github.com/stretchr/testify/require"
)

// TestStore wraps a SQLite database for testing.
// It provides a temporary database file that is cleaned up on close.
// Refs: MGIT-1.2.5, NFR-4, FR-4
type TestStore struct {
	DB   *sql.DB
	path string
}

// Path returns the filesystem path to the SQLite database file.
func (s *TestStore) Path() string {
	return s.path
}

// Close closes the database connection.
func (s *TestStore) Close() error {
	return s.DB.Close()
}

// NewTestStore creates a temporary SQLite database for testing.
// Returns a TestStore and a cleanup function.
// The database is configured with safety-critical PRAGMAs per CLAUDE.md.
// Refs: MGIT-1.2.5, NFR-3, NFR-4
func NewTestStore(t *testing.T) (*TestStore, func()) {
	t.Helper()

	tmpDir, err := os.MkdirTemp("", "mgit-test-store-*")
	require.NoError(t, err, "failed to create temp dir for test store")

	dbPath := filepath.Join(tmpDir, "index.db")

	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err, "failed to open test SQLite database")

	// Apply safety-critical PRAGMAs per CLAUDE.md SQL rules
	ctx := context.Background()
	pragmas := []string{
		"PRAGMA foreign_keys = ON",
		"PRAGMA journal_mode = WAL",
		"PRAGMA busy_timeout = 5000",
		"PRAGMA synchronous = FULL",
	}
	for _, pragma := range pragmas {
		_, err = db.ExecContext(ctx, pragma)
		require.NoError(t, err, "failed to execute pragma: %s", pragma)
	}

	store := &TestStore{
		DB:   db,
		path: dbPath,
	}

	cleanup := func() {
		_ = store.DB.Close()
		_ = os.RemoveAll(tmpDir)
	}

	return store, cleanup
}
