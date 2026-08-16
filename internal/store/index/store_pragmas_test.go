package index

import (
	"context"
	"database/sql"
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

	other := openWithBusyTimeout(t, dbPath)
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

	other := openWithBusyTimeout(t, dbPath)
	err = setJournalModeWAL(ctx, other, walRetryBudget)

	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}

// TestApplyPragma_ClosedDatabase_ReturnsError covers the non-retryable path:
// a failure that is not SQLITE_BUSY is returned at once, not retried until the
// budget runs out. Both the plain pragma and the WAL switch are checked.
// Refs: MGIT-121
func TestApplyPragma_ClosedDatabase_ReturnsError(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "index.db")

	db := openWithBusyTimeout(t, dbPath)
	require.NoError(t, db.Close())

	require.Error(t, applyPragma(ctx, db, "PRAGMA foreign_keys = ON"))

	start := time.Now()
	err := applyPragma(ctx, db, walPragma)
	require.Error(t, err)
	assert.Less(t, time.Since(start), time.Second, "a non-busy error must not be retried")
}
