package index

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
)

// driverName is the name modernc.org/sqlite registers itself under.
// The driver is registered by the blank import in store.go.
const driverName = "sqlite"

// pragmaConnector opens SQLite connections that already carry mgit's PRAGMAs.
//
// PRAGMAs are per-CONNECTION state, and a database/sql pool opens its
// connections lazily and independently: the first when the pool is first used,
// the rest under concurrent load, plus a replacement for any connection a
// driver error retired. Applying the pragmas to the *sql.DB reaches exactly
// ONE of them — database/sql routes an Exec to a single pooled connection — so
// every other connection reached the database with busy_timeout at SQLite's
// default of zero and foreign_keys off. A read that met a writer's lock on one
// of those failed INSTANTLY with SQLITE_BUSY instead of waiting, which is the
// MGIT-121 failure on the path that carries concurrent readers, surfacing only
// when several are in flight (i.e. under fleet load).
//
// A connector is the one hook database/sql offers that runs on EVERY
// connection, including connections created long after the store was opened;
// pre-warming the pool at open would have left replacements bare. The DSN
// alternative (file:...?_pragma=...) is deliberately not used: dbPath is an
// arbitrary host path and would have to be URI-escaped, which is its own hazard
// on Windows.
//
// What is applied, and in what order, stays with pragmaStatements (pragmas.go).
// This type only changes WHERE it is applied.
// Refs: MGIT-121.1, MGIT-121, CLAUDE.md SQL Rule 2
type pragmaConnector struct {
	driver driver.Driver
	dsn    string
}

// newPragmaConnector builds a connector for the database at dsn.
// Refs: MGIT-121.1
func newPragmaConnector(dsn string) (*pragmaConnector, error) {
	drv, err := sqliteDriver()
	if err != nil {
		return nil, err
	}
	return &pragmaConnector{driver: drv, dsn: dsn}, nil
}

// Driver implements driver.Connector.
func (c *pragmaConnector) Driver() driver.Driver { return c.driver }

// Connect opens a connection and applies the full pragma sequence to it before
// database/sql can hand it to anyone. A connection whose pragmas cannot be
// applied is closed and the error returned, so the pool never serves a
// half-configured connection.
// Refs: MGIT-121.1, CLAUDE.md SQL Rule 2
func (c *pragmaConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.driver.Open(c.dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite connection: %w", err)
	}

	pc, err := newPragmaConn(conn)
	if err != nil {
		// Best effort: the connect has already failed, and the close error
		// would only mask the reason it failed.
		_ = conn.Close()
		return nil, err
	}
	for _, pragma := range pragmaStatements() {
		if err := applyPragma(ctx, pc, pragma); err != nil {
			_ = conn.Close() // as above
			return nil, fmt.Errorf("pragma %q: %w", pragma, err)
		}
	}
	return conn, nil
}

// sqliteDriver returns the driver.Driver that modernc.org/sqlite registered
// under driverName.
//
// It is read out of database/sql's registry rather than constructed:
// &sqlite.Driver{} would skip the package's own initialization of its
// unexported fields. Going through the registry means a connector opens
// connections through exactly the same driver instance sql.Open(driverName, …)
// used before MGIT-121.1, so nothing but the pragma timing changes. sql.Open
// does not dial, so the throwaway handle never touches a database.
// Refs: MGIT-121.1
func sqliteDriver() (driver.Driver, error) {
	probe, err := sql.Open(driverName, "")
	if err != nil {
		return nil, fmt.Errorf("resolve %q driver: %w", driverName, err)
	}
	drv := probe.Driver()
	if err := probe.Close(); err != nil {
		return nil, fmt.Errorf("close %q driver probe: %w", driverName, err)
	}
	return drv, nil
}

// pragmaConn is the statement surface the pragma sequence needs. It is one
// identified connection, never a pool: that a statement lands on a known
// connection is the entire point (see pragmaConnector).
// Refs: MGIT-121.1
type pragmaConn interface {
	// execPragma runs a pragma that returns nothing worth reading.
	execPragma(ctx context.Context, stmt string) error
	// queryPragma runs a pragma and returns its first column as text.
	queryPragma(ctx context.Context, stmt string) (string, error)
}

// sqliteStmtConn is the subset of the optional driver interfaces that
// modernc.org/sqlite's connection implements and the pragma sequence uses.
// Without them a statement would have to be prepared and closed by hand.
type sqliteStmtConn interface {
	driver.ExecerContext
	driver.QueryerContext
}

// driverPragmaConn adapts a raw driver connection to pragmaConn.
type driverPragmaConn struct {
	conn sqliteStmtConn
}

// newPragmaConn adapts conn for the pragma sequence, or reports that this
// driver connection cannot run statements directly.
// Refs: MGIT-121.1
func newPragmaConn(conn driver.Conn) (pragmaConn, error) {
	stmtConn, ok := conn.(sqliteStmtConn)
	if !ok {
		return nil, fmt.Errorf(
			"sqlite driver connection %T supports neither ExecerContext nor QueryerContext", conn)
	}
	return driverPragmaConn{conn: stmtConn}, nil
}

// execPragma implements pragmaConn. The error is returned unwrapped so callers
// can still classify it (see isBusy).
func (c driverPragmaConn) execPragma(ctx context.Context, stmt string) error {
	// PRAGMAs are compile-time constants from pragmas.go, never user input, so
	// there are no arguments to bind. Refs: CLAUDE.md SQL Rule 1
	if _, err := c.conn.ExecContext(ctx, stmt, nil); err != nil {
		return err
	}
	return nil
}

// queryPragma implements pragmaConn, returning the first column of the first
// row. The error is returned unwrapped so callers can still classify it.
func (c driverPragmaConn) queryPragma(ctx context.Context, stmt string) (string, error) {
	// No arguments to bind, as in execPragma.
	rows, err := c.conn.QueryContext(ctx, stmt, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = rows.Close() }() // read fully below; nothing to report

	dest := make([]driver.Value, len(rows.Columns()))
	if len(dest) == 0 {
		return "", fmt.Errorf("%q returned no columns", stmt)
	}
	if err := rows.Next(dest); err != nil {
		if errors.Is(err, io.EOF) {
			return "", fmt.Errorf("%q returned no rows", stmt)
		}
		return "", err
	}
	return pragmaText(stmt, dest[0])
}

// pragmaText renders a pragma's value as text. SQLite hands text back as
// either string or []byte depending on how the driver allocated it.
func pragmaText(stmt string, value driver.Value) (string, error) {
	switch v := value.(type) {
	case string:
		return v, nil
	case []byte:
		return string(v), nil
	default:
		return "", fmt.Errorf("%q returned %T, want text", stmt, value)
	}
}
