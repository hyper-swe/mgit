package index

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// errStub is returned by the stub driver for anything a test did not script.
var errStub = errors.New("stub: not scripted")

// stubConn is a driver.Conn that can run no statements at all — it implements
// neither ExecerContext nor QueryerContext — so it exercises the branch where a
// driver connection cannot serve the pragma sequence.
type stubConn struct {
	closed bool
}

func (c *stubConn) Prepare(string) (driver.Stmt, error) { return nil, errStub }
func (c *stubConn) Begin() (driver.Tx, error)           { return nil, errStub }
func (c *stubConn) Close() error                        { c.closed = true; return nil }

// scriptedConn is a driver.Conn whose statements return canned results, so the
// pragma sequence's failure paths can be driven without a real database.
type scriptedConn struct {
	stubConn
	execErr  error
	queryErr error
	rows     driver.Rows
	execs    []string
}

func (c *scriptedConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	c.execs = append(c.execs, query)
	if c.execErr != nil {
		return nil, c.execErr
	}
	return driver.RowsAffected(0), nil
}

func (c *scriptedConn) QueryContext(_ context.Context, _ string, _ []driver.NamedValue) (driver.Rows, error) {
	if c.queryErr != nil {
		return nil, c.queryErr
	}
	return c.rows, nil
}

// valueRows is a driver.Rows over a fixed table of values.
type valueRows struct {
	cols   []string
	values [][]driver.Value
	next   int
}

func (r *valueRows) Columns() []string { return r.cols }
func (r *valueRows) Close() error      { return nil }
func (r *valueRows) Next(dest []driver.Value) error {
	if r.next >= len(r.values) {
		return io.EOF
	}
	copy(dest, r.values[r.next])
	r.next++
	return nil
}

// stubDriver hands out a scripted connection, or fails to open one.
type stubDriver struct {
	conn    driver.Conn
	openErr error
}

func (d stubDriver) Open(string) (driver.Conn, error) {
	if d.openErr != nil {
		return nil, d.openErr
	}
	return d.conn, nil
}

// countingDriver wraps a real driver and records every statement each
// connection runs, so a test can say what Connect actually costs.
type countingDriver struct {
	inner driver.Driver
	mu    *sync.Mutex
	stmts *[][]string // one entry per connection, in open order
}

func newCountingDriver(t *testing.T) countingDriver {
	t.Helper()
	inner, err := sqliteDriver()
	require.NoError(t, err)
	return countingDriver{inner: inner, mu: &sync.Mutex{}, stmts: &[][]string{}}
}

// statements returns the statements each connection ran, in open order.
func (d countingDriver) statements() [][]string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([][]string, len(*d.stmts))
	copy(out, *d.stmts)
	return out
}

func (d countingDriver) Open(name string) (driver.Conn, error) {
	inner, err := d.inner.Open(name)
	if err != nil {
		return nil, err
	}
	stmtConn, ok := inner.(sqliteStmtConn)
	if !ok {
		return nil, errStub
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	*d.stmts = append(*d.stmts, nil)
	return &countingConn{Conn: inner, stmtConn: stmtConn, driver: d, index: len(*d.stmts) - 1}, nil
}

// countingConn records the statements it is asked to run, then delegates.
type countingConn struct {
	driver.Conn
	stmtConn sqliteStmtConn
	driver   countingDriver
	index    int
}

func (c *countingConn) record(query string) {
	c.driver.mu.Lock()
	defer c.driver.mu.Unlock()
	(*c.driver.stmts)[c.index] = append((*c.driver.stmts)[c.index], query)
}

func (c *countingConn) ExecContext(ctx context.Context, q string, a []driver.NamedValue) (driver.Result, error) {
	c.record(q)
	return c.stmtConn.ExecContext(ctx, q, a)
}

func (c *countingConn) QueryContext(ctx context.Context, q string, a []driver.NamedValue) (driver.Rows, error) {
	c.record(q)
	return c.stmtConn.QueryContext(ctx, q, a)
}

// TestPragmaConnector_ConnectToWALDatabase_DoesNotRewriteJournalMode measures
// what the per-connection application costs now that Connect runs on EVERY new
// pooled connection, rather than assuming it is cheap.
//
// WAL is a persistent property of the database file, so setJournalModeWAL's
// first act — reading the current mode — short-circuits on every connection
// after the one that converted the database. The cost of a later connect is
// therefore three per-connection pragma writes plus ONE journal_mode read, and
// crucially NOT a second attempt at `PRAGMA journal_mode = WAL`, which is the
// statement that asks for a database-wide exclusive lock and the only one here
// that could contend with a concurrent writer.
// Refs: MGIT-121.1
func TestPragmaConnector_ConnectToWALDatabase_DoesNotRewriteJournalMode(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "index.db")

	// The store's own open converts the fresh database to WAL.
	store, err := New(dbPath, fixedClock())
	require.NoError(t, err)
	defer func() { _ = store.Close() }()

	// Now connect through an instrumented connector against that same,
	// already-converted database — the state every connection after the first
	// finds, whichever pool opens it.
	drv := newCountingDriver(t)
	db := sql.OpenDB(&pragmaConnector{driver: drv, dsn: dbPath})
	defer func() { _ = db.Close() }()

	conn, err := db.Conn(ctx)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()

	stmts := drv.statements()
	require.Len(t, stmts, 1, "expected exactly one connection to be opened")
	assert.Equal(t, []string{
		"PRAGMA busy_timeout = 5000",
		"PRAGMA foreign_keys = ON",
		"PRAGMA journal_mode", // the read that short-circuits the WAL switch
		"PRAGMA synchronous = FULL",
	}, stmts[0])
	assert.NotContains(t, stmts[0], walPragma,
		"connecting to an already-WAL database must not ask for the exclusive lock again")
}

// openRawPragmaConn opens ONE raw driver connection on dbPath with mgit's
// busy_timeout applied. This is what the pragma sequence runs against in
// production now that it is applied per connection (see connector.go), so the
// contention tests drive the real path.
func openRawPragmaConn(t *testing.T, dbPath string) pragmaConn {
	t.Helper()

	drv, err := sqliteDriver()
	require.NoError(t, err)
	raw, err := drv.Open(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = raw.Close() })

	conn, err := newPragmaConn(raw)
	require.NoError(t, err)
	require.NoError(t, conn.execPragma(context.Background(), pragmaStatements()[0]))
	return conn
}

// TestSQLiteDriver_ReturnsRegisteredDriver checks that the connector opens
// connections through the same driver instance sql.Open("sqlite", …) uses,
// rather than a zero-value one whose unexported state was never initialized.
// Refs: MGIT-121.1
func TestSQLiteDriver_ReturnsRegisteredDriver(t *testing.T) {
	drv, err := sqliteDriver()
	require.NoError(t, err)
	require.NotNil(t, drv)

	conn, err := drv.Open(filepath.Join(t.TempDir(), "index.db"))
	require.NoError(t, err)
	require.NoError(t, conn.Close())
}

// TestPragmaConnector_Connect_AppliesEveryPragmaInOrder pins that Connect runs
// the pragma sequence itself, in pragmaStatements' order, on the connection it
// is about to hand back — busy_timeout first (MGIT-121).
// Refs: MGIT-121.1, MGIT-121
func TestPragmaConnector_Connect_AppliesEveryPragmaInOrder(t *testing.T) {
	// journal_mode reads back as WAL, so the WAL switch is a no-op read.
	conn := &scriptedConn{rows: &valueRows{
		cols:   []string{"journal_mode"},
		values: [][]driver.Value{{"wal"}},
	}}
	connector := &pragmaConnector{driver: stubDriver{conn: conn}, dsn: "ignored"}

	got, err := connector.Connect(context.Background())
	require.NoError(t, err)
	assert.Same(t, conn, got)
	assert.False(t, conn.closed, "a healthy connection must be handed back open")

	// Every pragma except the WAL switch is executed; the WAL switch goes
	// through the query path because it reports the resulting mode.
	var want []string
	for _, pragma := range pragmaStatements() {
		if pragma != walPragma {
			want = append(want, pragma)
		}
	}
	assert.Equal(t, want, conn.execs)
}

// TestPragmaConnector_Connect_DriverOpenFails_ReturnsError covers the open
// failure path.
// Refs: MGIT-121.1
func TestPragmaConnector_Connect_DriverOpenFails_ReturnsError(t *testing.T) {
	boom := errors.New("no such file")
	connector := &pragmaConnector{driver: stubDriver{openErr: boom}, dsn: "ignored"}

	_, err := connector.Connect(context.Background())
	require.ErrorIs(t, err, boom)
	assert.Contains(t, err.Error(), "open sqlite connection")
}

// TestPragmaConnector_Connect_PragmaFails_ClosesConnection is the safety
// property: a connection whose pragmas did not apply must never reach the pool,
// because that is precisely the half-configured connection MGIT-121.1 is about.
// Refs: MGIT-121.1
func TestPragmaConnector_Connect_PragmaFails_ClosesConnection(t *testing.T) {
	boom := errors.New("disk I/O error")
	conn := &scriptedConn{execErr: boom}
	connector := &pragmaConnector{driver: stubDriver{conn: conn}, dsn: "ignored"}

	_, err := connector.Connect(context.Background())
	require.ErrorIs(t, err, boom)
	assert.Contains(t, err.Error(), pragmaStatements()[0], "the failing pragma must be named")
	assert.True(t, conn.closed, "a connection that failed its pragmas must be closed, not leaked")
}

// TestPragmaConnector_Connect_ConnWithoutContextStatements_ReturnsError covers
// a driver connection that cannot run statements directly. It is defensive —
// modernc.org/sqlite's connection implements both interfaces — but the
// alternative to checking is a type-assertion panic inside database/sql.
// Refs: MGIT-121.1
func TestPragmaConnector_Connect_ConnWithoutContextStatements_ReturnsError(t *testing.T) {
	conn := &stubConn{}
	connector := &pragmaConnector{driver: stubDriver{conn: conn}, dsn: "ignored"}

	_, err := connector.Connect(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ExecerContext")
	assert.True(t, conn.closed, "the unusable connection must be closed, not leaked")
}

// TestPragmaConnector_Driver_ReturnsWrappedDriver covers driver.Connector's
// other method, which database/sql uses for DB.Driver().
// Refs: MGIT-121.1
func TestPragmaConnector_Driver_ReturnsWrappedDriver(t *testing.T) {
	drv := stubDriver{conn: &stubConn{}}
	connector := &pragmaConnector{driver: drv, dsn: "ignored"}
	assert.Equal(t, drv, connector.Driver())
}

// TestQueryPragma_MalformedResults_ReturnError covers the readback paths that
// cannot be reached with a real SQLite: a pragma answering with no columns, no
// rows, or a non-text value. Each must be reported rather than silently read as
// an empty mode, which would send setJournalModeWAL round its retry loop.
// Refs: MGIT-121.1
func TestQueryPragma_MalformedResults_ReturnError(t *testing.T) {
	queryBoom := errors.New("query failed")
	tests := []struct {
		name    string
		conn    *scriptedConn
		wantErr string
	}{
		{
			name:    "query_error",
			conn:    &scriptedConn{queryErr: queryBoom},
			wantErr: queryBoom.Error(),
		},
		{
			name:    "no_columns",
			conn:    &scriptedConn{rows: &valueRows{}},
			wantErr: "returned no columns",
		},
		{
			name:    "no_rows",
			conn:    &scriptedConn{rows: &valueRows{cols: []string{"journal_mode"}}},
			wantErr: "returned no rows",
		},
		{
			name: "non_text_value",
			conn: &scriptedConn{rows: &valueRows{
				cols:   []string{"journal_mode"},
				values: [][]driver.Value{{int64(3)}},
			}},
			wantErr: "want text",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn, err := newPragmaConn(tt.conn)
			require.NoError(t, err)

			_, err = conn.queryPragma(context.Background(), "PRAGMA journal_mode")
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

// TestQueryPragma_BytesValue_ReadsAsText covers SQLite text arriving as []byte
// rather than string, which depends on how the driver allocated the column.
// Refs: MGIT-121.1
func TestQueryPragma_BytesValue_ReadsAsText(t *testing.T) {
	conn, err := newPragmaConn(&scriptedConn{rows: &valueRows{
		cols:   []string{"journal_mode"},
		values: [][]driver.Value{{[]byte("wal")}},
	}})
	require.NoError(t, err)

	mode, err := conn.queryPragma(context.Background(), "PRAGMA journal_mode")
	require.NoError(t, err)
	assert.Equal(t, "wal", mode)
}
