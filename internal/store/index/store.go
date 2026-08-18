package index

import (
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
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

	// ulidMu guards ulidEntropy and ulidLastMs: monotonic entropy keeps
	// ULIDs strictly increasing within one millisecond (and under a
	// frozen test clock), and the millisecond clamp keeps them
	// increasing even if the wall clock steps backwards (NTP, VM
	// resume), so append-ordered audit ids always sort correctly within
	// this process. Cross-process ordering is serialized by the
	// single-writer pool plus the process-level file lock (MGIT-10.1).
	// Refs: FR-17.18
	ulidMu      sync.Mutex
	ulidEntropy *ulid.MonotonicEntropy
	ulidLastMs  uint64
}

// New creates or opens a Store at the given database path.
// The safety-critical PRAGMAs required by CLAUDE.md SQL Rule 2 are applied by
// the connector openPools installs, so they hold on every connection either
// pool ever opens, not just the first (see connector.go).
// Refs: NFR-3, MGIT-2.3.2, MGIT-121.1
func New(dbPath string, clock func() time.Time) (*Store, error) {
	if clock == nil {
		return nil, fmt.Errorf("clock must not be nil")
	}
	// Create the directory the database lives in, as every other store in
	// this repo does for its own state. SQLite reports a missing parent as
	// "unable to open database file: out of memory (14)", which names neither
	// the path nor the problem, and leaving that to callers means each one
	// re-learns it the hard way. 0700: these files are audit and index state.
	// Refs: FR-17.13, MGIT-61.15
	if dir := filepath.Dir(dbPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create index directory %s: %w", dir, err)
		}
	}

	writeDB, readDB, err := openPools(dbPath)
	if err != nil {
		return nil, err
	}

	store := &Store{
		readDB:      readDB,
		writeDB:     writeDB,
		dbPath:      dbPath,
		clock:       clock,
		ulidEntropy: ulid.Monotonic(rand.Reader, 0),
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

// newULID returns a monotonically increasing ULID for audit-row ids.
// The timestamp never decreases across calls (backwards wall-clock
// steps are clamped), so ORDER BY id is append order in this process.
func (s *Store) newULID() (string, error) {
	s.ulidMu.Lock()
	defer s.ulidMu.Unlock()

	ms := ulid.Timestamp(s.clock().UTC())
	if ms < s.ulidLastMs {
		ms = s.ulidLastMs
	}
	s.ulidLastMs = ms

	id, err := ulid.New(ms, s.ulidEntropy)
	if err != nil {
		return "", fmt.Errorf("new ulid: %w", err)
	}
	return id.String(), nil
}

// WriteTx executes fn within a serialized write transaction.
// If fn returns an error, the transaction is rolled back.
// The transaction is opened at SERIALIZABLE isolation per CLAUDE.md SQL Rule 3;
// the single-writer pool (writeDB.SetMaxOpenConns(1)) already serializes
// writers, so this is belt-and-braces — modernc/sqlite maps it onto SQLite's
// serializable semantics. Refs: NFR-3, CLAUDE.md SQL Rule 3
func (s *Store) WriteTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.writeDB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
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

// Pool sizes: one writer for mutual exclusion, several readers for concurrency.
const (
	maxWriteConns = 1
	maxReadConns  = 5
)

// openPools opens the write and read pools over dbPath.
//
// Both go through a pragmaConnector, so the safety-critical PRAGMAs are applied
// to EVERY connection either pool opens — including the ones opened lazily
// under load and the ones opened to replace a connection a driver error retired
// — rather than to whichever single connection a pool-level Exec landed on
// (MGIT-121.1).
//
// Each pool is then pinged, because sql.OpenDB is lazy and nothing has
// connected yet: an unopenable database, or a pragma that will not apply, must
// fail HERE at New as it did when the pragmas were applied to the pool, not on
// some later first use.
// Refs: MGIT-121.1, MGIT-121, NFR-3, CLAUDE.md SQL Rule 2, CLAUDE.md SQL Rule 4
func openPools(dbPath string) (*sql.DB, *sql.DB, error) {
	connector, err := newPragmaConnector(dbPath)
	if err != nil {
		return nil, nil, err
	}

	// One connector, two pools: it is stateless, and both pools address the
	// same database file with the same pragmas.
	writeDB := sql.OpenDB(connector)
	writeDB.SetMaxOpenConns(maxWriteConns)
	readDB := sql.OpenDB(connector)
	readDB.SetMaxOpenConns(maxReadConns)

	ctx := context.Background()
	for _, pool := range []struct {
		name string
		db   *sql.DB
	}{{"write", writeDB}, {"read", readDB}} {
		if err := pool.db.PingContext(ctx); err != nil {
			_ = writeDB.Close() // already failing; a close error would mask why
			_ = readDB.Close()
			return nil, nil, fmt.Errorf("open %s db: %w", pool.name, err)
		}
	}
	return writeDB, readDB, nil
}

// migrate runs schema migrations to the current version.
func (s *Store) migrate() error {
	ctx := context.Background()

	// Create all tables
	if _, err := s.writeDB.ExecContext(ctx, createTablesSQL); err != nil {
		return fmt.Errorf("create tables: %w", err)
	}

	// Additive column migrations for databases created before the
	// column existed (ALTER ADD COLUMN only — existing rows are never
	// updated or deleted; new columns read back NULL). Refs: FR-17.18
	if err := s.ensureColumn(ctx, "task_commits", "sandbox_id", "TEXT"); err != nil {
		return err
	}
	// ADR-008 (MGIT-35): pin each task's fork-base in the worktree registry.
	if err := s.ensureColumn(ctx, "worktrees", "fork_base", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if _, err := s.writeDB.ExecContext(ctx, postMigrationIndexSQL); err != nil {
		return fmt.Errorf("create post-migration indexes: %w", err)
	}

	// Append a schema_version row whenever the recorded version is
	// behind (version history is itself append-only).
	var latest int
	err := s.readDB.QueryRowContext(ctx,
		"SELECT COALESCE(MAX(version), 0) FROM schema_version").Scan(&latest)
	if err != nil {
		return fmt.Errorf("check schema version: %w", err)
	}

	if latest < schemaVersion {
		_, err := s.writeDB.ExecContext(ctx,
			"INSERT INTO schema_version (version, applied_at) VALUES (?, ?)",
			schemaVersion, s.clock().UTC().Format(time.RFC3339))
		if err != nil {
			return fmt.Errorf("insert schema version: %w", err)
		}
	}

	return nil
}

// ensureColumn adds a column to an existing table if absent. Purely
// additive: never rewrites rows, so append-only laws are preserved.
func (s *Store) ensureColumn(ctx context.Context, table, column, colType string) error {
	// PRAGMA table_info cannot be parameterized; table/column names here
	// are compile-time constants from this package, never user input.
	rows, err := s.readDB.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return fmt.Errorf("table info %s: %w", table, err)
	}
	defer rows.Close() //nolint:errcheck // non-critical

	for rows.Next() {
		var cid, notNull, pk int
		var name, declType string
		var dflt any
		if err := rows.Scan(&cid, &name, &declType, &notNull, &dflt, &pk); err != nil {
			return fmt.Errorf("scan table info: %w", err)
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("table info rows: %w", err)
	}

	if _, err := s.writeDB.ExecContext(ctx,
		"ALTER TABLE "+table+" ADD COLUMN "+column+" "+colType); err != nil {
		return fmt.Errorf("add column %s.%s: %w", table, column, err)
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
