package index

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// openWithBusyTimeout opens a single-connection pool on dbPath with the
// busy_timeout mgit uses, so contention behavior matches production.
func openWithBusyTimeout(t *testing.T, dbPath string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	_, err = db.ExecContext(context.Background(),
		fmt.Sprintf("PRAGMA busy_timeout = %d", busyTimeoutMillis))
	require.NoError(t, err)
	return db
}

// holdWriteLock takes a write lock on db and releases it after holdFor,
// simulating a sibling process mid-write. The returned func releases early and
// is safe to call twice, so callers can defer it.
func holdWriteLock(ctx context.Context, t *testing.T, db *sql.DB, holdFor time.Duration) func() {
	t.Helper()
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	// The INSERT is what upgrades the transaction to a RESERVED lock; BEGIN
	// alone is deferred and locks nothing.
	_, err = tx.ExecContext(ctx, "INSERT INTO lock_canary (id) VALUES (1)")
	require.NoError(t, err)

	var once sync.Once
	release := func() { once.Do(func() { _ = tx.Rollback() }) }
	go func() {
		time.Sleep(holdFor)
		release()
	}()
	return release
}

// TestNew_ContendedOpen_WaitsForLockInsteadOfFailing is the deterministic
// regression for MGIT-121. Another connection holds a write lock on the
// database while New runs; switching the database into WAL mode needs an
// exclusive lock, so the open must WAIT for the holder to let go rather than
// return SQLITE_BUSY immediately.
//
// It goes red both ways this can be got wrong: with the pre-MGIT-121 pragma
// ordering (journal_mode before busy_timeout), and with the correct ordering
// but no retry around the WAL switch — busy_timeout does not cover that
// statement (see TestSetJournalModeWAL_BusyTimeoutAloneDoesNotCoverIt). Both
// fail here with "database is locked (5) (SQLITE_BUSY)".
// Refs: MGIT-121, CLAUDE.md SQL Rule 2
func TestNew_ContendedOpen_WaitsForLockInsteadOfFailing(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "index.db")

	// Create the database ahead of the open so it exists in rollback-journal
	// mode: that is the state the daemon's audit index is in the first time a
	// fleet races to open it, and it is the state whose WAL conversion locks.
	holder := openWithBusyTimeout(t, dbPath)
	_, err := holder.ExecContext(ctx, "CREATE TABLE lock_canary (id INTEGER PRIMARY KEY)")
	require.NoError(t, err)

	// Take a write lock and hold it briefly, well inside the retry budget.
	const holdFor = 300 * time.Millisecond
	defer holdWriteLock(ctx, t, holder, holdFor)()

	start := time.Now()
	store, err := New(dbPath, fixedClock())
	require.NoError(t, err, "open must wait out a concurrent lock holder, not fail with SQLITE_BUSY")
	defer func() { _ = store.Close() }()

	assert.GreaterOrEqual(t, time.Since(start), holdFor/2,
		"the open returned before the lock holder released, so it never contended")
}

// TestSetJournalModeWAL_BusyTimeoutAloneDoesNotCoverIt pins the SQLite
// behavior that makes setJournalModeWAL's retry loop necessary: the exclusive
// lock a journal-mode change needs is taken WITHOUT consulting the busy
// handler, so `PRAGMA journal_mode = WAL` is refused immediately even on a
// connection with a 5s busy_timeout — while an ordinary write on that same
// connection waits, which is the control that proves the timeout is in force.
//
// If a future SQLite makes journal_mode honor busy_timeout, this test goes
// red and the retry loop can be deleted. Until then, hoisting busy_timeout is
// necessary but not sufficient, and deleting the loop reopens MGIT-121.
// Refs: MGIT-121
func TestSetJournalModeWAL_BusyTimeoutAloneDoesNotCoverIt(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "index.db")

	holder := openWithBusyTimeout(t, dbPath)
	_, err := holder.ExecContext(ctx, "CREATE TABLE lock_canary (id INTEGER PRIMARY KEY)")
	require.NoError(t, err)
	release := holdWriteLock(ctx, t, holder, 300*time.Millisecond)
	defer release()

	other := openWithBusyTimeout(t, dbPath)

	start := time.Now()
	var mode string
	walErr := other.QueryRowContext(ctx, "PRAGMA journal_mode = WAL").Scan(&mode)
	walWait := time.Since(start)

	require.Error(t, walErr, "expected the WAL switch to be refused while the lock is held")
	assert.Less(t, walWait, time.Second,
		"the WAL switch failed but did not wait, i.e. busy_timeout did not apply to it")

	// Control: the same connection's ordinary write DOES wait out the holder.
	start = time.Now()
	_, err = other.ExecContext(ctx, "INSERT INTO lock_canary (id) VALUES (2)")
	require.NoError(t, err, "busy_timeout was not in force on this connection")
	assert.Greater(t, time.Since(start), walWait,
		"the write should have waited for the lock, unlike the WAL switch")
}

// TestNew_ConcurrentOpensOfSameIndex_AllSucceed is the field shape from the
// MGIT-113 fleet soak: several processes (here, goroutines) open the same
// index at once, each converting it to WAL, and every one must come up. A
// single SQLITE_BUSY here is fatal to mgit-sandboxd, which exits 2 on a
// wiring failure and takes the whole fleet's bring-up with it.
// Refs: MGIT-121, MGIT-113, NFR-17.6
func TestNew_ConcurrentOpensOfSameIndex_AllSucceed(t *testing.T) {
	const openers = 8
	dbPath := filepath.Join(t.TempDir(), "index.db")

	var start sync.WaitGroup
	start.Add(1)
	var done sync.WaitGroup
	errs := make([]error, openers)
	stores := make([]*Store, openers)

	for i := range openers {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait() // release every opener into the same instant
			stores[i], errs[i] = New(dbPath, fixedClock())
		}()
	}
	start.Done()
	done.Wait()

	for i, err := range errs {
		assert.NoError(t, err, "opener %d failed; concurrent opens of one index must all succeed", i)
		if stores[i] != nil {
			_ = stores[i].Close()
		}
	}
}

// TestNew_Pragmas_AreInForceOnBothPools reads the pragmas back rather than
// trusting that the statements executed. busy_timeout in particular is silent
// when it is wrong: a connection with the default of zero looks identical
// until something else holds a lock.
// Refs: MGIT-121, CLAUDE.md SQL Rule 2
func TestNew_Pragmas_AreInForceOnBothPools(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	pools := map[string]*sql.DB{"write": store.writeDB, "read": store.readDB}
	for name, db := range pools {
		t.Run(name, func(t *testing.T) {
			var busyTimeout int
			require.NoError(t, db.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&busyTimeout))
			assert.Equal(t, busyTimeoutMillis, busyTimeout, "busy_timeout not in force")

			var journalMode string
			require.NoError(t, db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journalMode))
			assert.Equal(t, "wal", journalMode)

			var foreignKeys int
			require.NoError(t, db.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&foreignKeys))
			assert.Equal(t, 1, foreignKeys)

			// synchronous is reported numerically: 0=OFF, 1=NORMAL, 2=FULL, 3=EXTRA.
			var synchronous int
			require.NoError(t, db.QueryRowContext(ctx, "PRAGMA synchronous").Scan(&synchronous))
			assert.Equal(t, 2, synchronous, "synchronous must be FULL")
		})
	}
}

// assertPragmasInForce reads the per-connection pragmas back FROM the given
// connection rather than trusting that a statement was executed somewhere on
// the pool. busy_timeout and foreign_keys are per-connection state: a pool-wide
// query answers from whichever connection the pool happens to hand out, so only
// a held *sql.Conn can say what an individual connection is carrying.
// Refs: MGIT-121.1, CLAUDE.md SQL Rule 2
func assertPragmasInForce(ctx context.Context, t *testing.T, conn *sql.Conn, label string) {
	t.Helper()

	var busyTimeout int
	require.NoError(t, conn.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&busyTimeout))
	assert.Equal(t, busyTimeoutMillis, busyTimeout,
		"%s: busy_timeout is %d, so a lock met here fails instantly instead of waiting",
		label, busyTimeout)

	var foreignKeys int
	require.NoError(t, conn.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&foreignKeys))
	assert.Equal(t, 1, foreignKeys, "%s: foreign_keys not in force", label)

	// synchronous is reported numerically: 0=OFF, 1=NORMAL, 2=FULL, 3=EXTRA.
	var synchronous int
	require.NoError(t, conn.QueryRowContext(ctx, "PRAGMA synchronous").Scan(&synchronous))
	assert.Equal(t, 2, synchronous, "%s: synchronous must be FULL", label)

	// journal_mode is a persistent database property, not per-connection, so it
	// is checked for completeness rather than as evidence about this connection.
	var mode string
	require.NoError(t, conn.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode))
	assert.Equal(t, "wal", mode, "%s: journal_mode", label)
}

// takeAllConns grabs MaxOpenConns *sql.Conn from db and holds them, which
// forces the pool to open every connection it is allowed to. Each *sql.Conn
// owns its underlying connection exclusively for its lifetime, so holding them
// all at once guarantees they are DISTINCT.
func takeAllConns(ctx context.Context, t *testing.T, db *sql.DB) []*sql.Conn {
	t.Helper()

	maxConns := db.Stats().MaxOpenConnections
	require.Positive(t, maxConns, "pool has no connection limit to exhaust")

	conns := make([]*sql.Conn, 0, maxConns)
	for range maxConns {
		conn, err := db.Conn(ctx)
		require.NoError(t, err)
		conns = append(conns, conn)
	}
	t.Cleanup(func() {
		for _, conn := range conns {
			_ = conn.Close()
		}
	})

	require.Equal(t, maxConns, db.Stats().OpenConnections,
		"expected %d distinct connections to be open", maxConns)
	return conns
}

// TestNew_EveryPooledConnection_HasPragmas is the regression for MGIT-121.1.
// Applying pragmas with db.ExecContext sets them on exactly ONE pooled
// connection, because database/sql routes that Exec to a single connection and
// pragmas are per-connection state. The pool's other connections are opened
// later, lazily, under concurrent load — bare, with busy_timeout back at its
// default of zero, which is the MGIT-121 failure on the path that carries
// concurrent readers.
//
// Every connection either pool can hand out must carry the pragmas, so the test
// takes them all at once and asks each one directly.
// Refs: MGIT-121.1, MGIT-121, CLAUDE.md SQL Rule 2
func TestNew_EveryPooledConnection_HasPragmas(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	pools := []struct {
		name string
		db   *sql.DB
	}{
		{"write", store.writeDB},
		{"read", store.readDB},
	}
	for _, pool := range pools {
		t.Run(pool.name, func(t *testing.T) {
			for i, conn := range takeAllConns(ctx, t, pool.db) {
				assertPragmasInForce(ctx, t, conn, fmt.Sprintf("%s conn %d", pool.name, i))
			}
		})
	}
}

// TestNew_ConnectionReplacedAfterDriverError_HasPragmas covers the case that
// pre-warming the pool at open would have missed, and the reason MGIT-121.1
// refused that shortcut: database/sql discards a connection that reported
// driver.ErrBadConn and opens a REPLACEMENT on demand. Bounding the pool
// (writeDB's MaxOpenConns(1)) bounds concurrency but does not pin identity, so
// the replacement is a brand-new connection that no open-time setup ever saw.
// Refs: MGIT-121.1
func TestNew_ConnectionReplacedAfterDriverError_HasPragmas(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	pools := []struct {
		name string
		db   *sql.DB
	}{
		{"write", store.writeDB},
		{"read", store.readDB},
	}
	for _, pool := range pools {
		t.Run(pool.name, func(t *testing.T) {
			// Poison one connection: returning ErrBadConn from Raw tells
			// database/sql the connection is unusable, so it is closed rather
			// than returned to the pool.
			doomed, err := pool.db.Conn(ctx)
			require.NoError(t, err)
			var doomedID string
			require.ErrorIs(t, doomed.Raw(func(driverConn any) error {
				doomedID = fmt.Sprintf("%p", driverConn)
				return driver.ErrBadConn
			}), driver.ErrBadConn)
			// Raw already discarded the connection on the way out, so Close is
			// either a no-op or reports it as done; both mean it is gone.
			if err := doomed.Close(); err != nil {
				require.ErrorIs(t, err, sql.ErrConnDone)
			}

			// The pool now has to open a fresh connection to serve this.
			replacement, err := pool.db.Conn(ctx)
			require.NoError(t, err)
			defer func() { _ = replacement.Close() }()

			var replacementID string
			require.NoError(t, replacement.Raw(func(driverConn any) error {
				replacementID = fmt.Sprintf("%p", driverConn)
				return nil
			}))
			require.NotEqual(t, doomedID, replacementID,
				"the poisoned connection was reused, so nothing was replaced")

			assertPragmasInForce(ctx, t, replacement, pool.name+" replacement conn")
		})
	}
}

// TestApplyPragmas_BusyTimeoutPrecedesLockTakingPragmas asserts the ordering
// itself, not just its effect. The effect is a race and can pass by luck; the
// ordering is wrong or right on reading. journal_mode is the pragma that takes
// a lock, so busy_timeout must appear before it.
// Refs: MGIT-121
func TestApplyPragmas_BusyTimeoutPrecedesLockTakingPragmas(t *testing.T) {
	busyAt, journalAt := -1, -1
	for i, pragma := range pragmaStatements() {
		switch {
		case strings.HasPrefix(pragma, "PRAGMA busy_timeout"):
			busyAt = i
		case strings.HasPrefix(pragma, "PRAGMA journal_mode"):
			journalAt = i
		}
	}

	require.NotEqual(t, -1, busyAt, "busy_timeout pragma missing")
	require.NotEqual(t, -1, journalAt, "journal_mode pragma missing")
	assert.Less(t, busyAt, journalAt,
		"busy_timeout must be set before journal_mode, which takes an exclusive lock")
}

// TestSetJournalModeWAL_BudgetExhausted_ReturnsBusyError covers the give-up
// path: a lock held longer than the budget is still a failure, and the error
// says how long was waited and carries the underlying SQLITE_BUSY.
// Refs: MGIT-121
func TestSetJournalModeWAL_BudgetExhausted_ReturnsBusyError(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "index.db")

	holder := openWithBusyTimeout(t, dbPath)
	_, err := holder.ExecContext(ctx, "CREATE TABLE lock_canary (id INTEGER PRIMARY KEY)")
	require.NoError(t, err)
	defer holdWriteLock(ctx, t, holder, 10*time.Second)()

	other := openRawPragmaConn(t, dbPath)
	err = setJournalModeWAL(ctx, other, 100*time.Millisecond)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "still locked after 100ms")
	assert.True(t, isBusy(err), "the underlying SQLITE_BUSY must survive wrapping")
}

// TestSetJournalModeWAL_CanceledContext_Aborts covers the cancellation path:
// a caller that gives up must not be held for the full budget.
// Refs: MGIT-121
func TestSetJournalModeWAL_CanceledContext_Aborts(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "index.db")

	holder := openWithBusyTimeout(t, dbPath)
	_, err := holder.ExecContext(context.Background(),
		"CREATE TABLE lock_canary (id INTEGER PRIMARY KEY)")
	require.NoError(t, err)
	defer holdWriteLock(context.Background(), t, holder, 10*time.Second)()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	other := openRawPragmaConn(t, dbPath)
	err = setJournalModeWAL(ctx, other, walRetryBudget)

	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}

// TestApplyPragma_NonBusyError_IsReturnedNotRetried covers the non-retryable
// path: a failure that is not SQLITE_BUSY is returned at once, not retried
// until the budget runs out. Both the plain pragma and the WAL switch are
// checked.
//
// The failure is scripted rather than provoked by closing a database: since
// MGIT-121.1 the pragma sequence runs on a RAW driver connection, and a closed
// modernc.org/sqlite connection has released the memory a further statement
// would touch, so the honest way to produce a non-busy error is to script one.
// Refs: MGIT-121.1, MGIT-121
func TestApplyPragma_NonBusyError_IsReturnedNotRetried(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("disk I/O error")

	conn, err := newPragmaConn(&scriptedConn{execErr: boom, queryErr: boom})
	require.NoError(t, err)

	require.ErrorIs(t, applyPragma(ctx, conn, "PRAGMA foreign_keys = ON"), boom)
	require.False(t, isBusy(boom), "the scripted failure must not look like SQLITE_BUSY")

	start := time.Now()
	err = applyPragma(ctx, conn, walPragma)
	require.ErrorIs(t, err, boom)
	assert.Less(t, time.Since(start), time.Second, "a non-busy error must not be retried")
}
