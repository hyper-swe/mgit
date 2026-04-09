package index

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // Pure Go SQLite driver (CGO-free per NFR-4)
)

// Store manages the SQLite task-commit index.
// It uses dual connection pools: one writer (serialized) and multiple readers.
// All write operations go through WriteTx for transaction safety.
// Refs: FR-4, NFR-3, MGIT-2.3.2
type Store struct {
	readDB  *sql.DB
	writeDB *sql.DB
	dbPath  string
	clock   func() time.Time
}

// New creates or opens a Store at the given database path.
// Configures safety-critical PRAGMAs per CLAUDE.md SQL rules.
// Refs: NFR-3, MGIT-2.3.2
func New(dbPath string, clock func() time.Time) (*Store, error) {
	if clock == nil {
		return nil, fmt.Errorf("clock must not be nil")
	}

	// Open write connection pool (single writer for mutual exclusion)
	writeDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open write db: %w", err)
	}
	writeDB.SetMaxOpenConns(1) // Enforce single-writer mutual exclusion

	// Open read connection pool (multiple readers)
	readDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		_ = writeDB.Close()
		return nil, fmt.Errorf("open read db: %w", err)
	}
	readDB.SetMaxOpenConns(5)

	store := &Store{
		readDB:  readDB,
		writeDB: writeDB,
		dbPath:  dbPath,
		clock:   clock,
	}

	// Apply safety-critical PRAGMAs
	if err := store.applyPragmas(); err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("apply pragmas: %w", err)
	}

	// Run migrations
	if err := store.migrate(); err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}

	return store, nil
}

// Close gracefully shuts down both connection pools.
func (s *Store) Close() error {
	writeErr := s.writeDB.Close()
	readErr := s.readDB.Close()
	if writeErr != nil {
		return writeErr
	}
	return readErr
}

// Path returns the database file path.
func (s *Store) Path() string {
	return s.dbPath
}

// Now returns the current time from the injected clock.
func (s *Store) Now() time.Time {
	return s.clock()
}

// WriteTx executes fn within a serialized write transaction.
// If fn returns an error, the transaction is rolled back.
// Refs: NFR-3
func (s *Store) WriteTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin write tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// ReadTx executes fn within a read-only transaction.
// Refs: NFR-3
func (s *Store) ReadTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.readDB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("begin read tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// applyPragmas sets safety-critical SQLite PRAGMAs on both pools.
// Refs: CLAUDE.md SQL Rule 2
func (s *Store) applyPragmas() error {
	ctx := context.Background()
	pragmas := []string{
		"PRAGMA foreign_keys = ON",
		"PRAGMA journal_mode = WAL",
		"PRAGMA busy_timeout = 5000",
		"PRAGMA synchronous = FULL",
	}

	for _, pragma := range pragmas {
		if _, err := s.writeDB.ExecContext(ctx, pragma); err != nil {
			return fmt.Errorf("write db pragma %q: %w", pragma, err)
		}
		if _, err := s.readDB.ExecContext(ctx, pragma); err != nil {
			return fmt.Errorf("read db pragma %q: %w", pragma, err)
		}
	}
	return nil
}

// migrate runs schema migrations to the current version.
func (s *Store) migrate() error {
	ctx := context.Background()

	// Create all tables
	if _, err := s.writeDB.ExecContext(ctx, createTablesSQL); err != nil {
		return fmt.Errorf("create tables: %w", err)
	}

	// Record schema version if not already present
	var count int
	err := s.readDB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM schema_version").Scan(&count)
	if err != nil {
		return fmt.Errorf("check schema version: %w", err)
	}

	if count == 0 {
		_, err := s.writeDB.ExecContext(ctx,
			"INSERT INTO schema_version (version, applied_at) VALUES (?, ?)",
			schemaVersion, s.clock().UTC().Format(time.RFC3339))
		if err != nil {
			return fmt.Errorf("insert schema version: %w", err)
		}
	}

	return nil
}

// SchemaVersion returns the current schema version.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var version int
	err := s.readDB.QueryRowContext(ctx,
		"SELECT version FROM schema_version ORDER BY version DESC LIMIT 1").Scan(&version)
	if err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	return version, nil
}
