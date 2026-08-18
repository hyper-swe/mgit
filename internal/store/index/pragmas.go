package index

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	sqlite "modernc.org/sqlite" // named for *sqlite.Error; the driver is registered in store.go
)

// busyTimeoutMillis is how long a connection waits for a lock another
// connection holds before giving up with SQLITE_BUSY. It is a per-connection
// setting that defaults to ZERO — "fail immediately" — so it must be set on
// every connection, and set before anything that can contend.
const busyTimeoutMillis = 5000

// walPragma switches the database into write-ahead logging. It is singled out
// because it is the one statement here that takes a database-wide lock, and
// the one SQLite's busy handler does not cover (see setJournalModeWAL).
const walPragma = "PRAGMA journal_mode = WAL"

// sqliteBusy is SQLITE_BUSY. SQLite also reports extended codes in the high
// bits (SQLITE_BUSY_SNAPSHOT = 517), so compare the low byte.
const sqliteBusy = 5

// WAL-conversion retry schedule. The budget matches busy_timeout so a caller
// sees one consistent "how long will an open wait for a lock" answer; the
// backoff starts short because the conversion window is milliseconds wide.
const (
	walRetryBudget  = busyTimeoutMillis * time.Millisecond
	walRetryInitial = 2 * time.Millisecond
	walRetryMax     = 50 * time.Millisecond
)

// pragmaStatements returns the safety-critical PRAGMAs in the order they must
// be applied.
//
// THE ORDER IS LOAD-BEARING: busy_timeout comes FIRST, before journal_mode and
// before the schema migration that follows. Until busy_timeout is set it is
// SQLite's default of zero, so a connection that meets a lock held by another
// process fails instantly with SQLITE_BUSY instead of waiting. That killed
// every mgit-sandboxd in a four-wide fleet bring-up (MGIT-121): the daemon
// exits 2 on a wiring failure, so one lost race takes the whole fleet with it.
// busy_timeout is per-connection and takes no lock, so hoisting it costs
// nothing. Anything added here that can block belongs BELOW it.
//
// journal_mode is the odd one out twice over. WAL is recorded in the database
// header and persists, so only the first opener of a database actually
// converts it — it is issued on every connection anyway, a cheap no-op after
// the first, so a database restored or copied out of WAL mode is put back.
// And its lock is NOT covered by busy_timeout, so it is applied through
// setJournalModeWAL rather than executed directly.
// Refs: MGIT-121, CLAUDE.md SQL Rule 2
func pragmaStatements() []string {
	return []string{
		fmt.Sprintf("PRAGMA busy_timeout = %d", busyTimeoutMillis),
		"PRAGMA foreign_keys = ON",
		walPragma,
		"PRAGMA synchronous = FULL",
	}
}

// applyPragma executes one pragma from pragmaStatements against a single
// connection, routing the WAL switch through its retry path.
//
// The receiver is one connection, not a pool: busy_timeout and foreign_keys
// are per-connection state, so a pool would leave every connection it did not
// happen to pick bare (MGIT-121.1). See pragmaConnector.
// Refs: MGIT-121.1, MGIT-121
func applyPragma(ctx context.Context, conn pragmaConn, pragma string) error {
	if pragma == walPragma {
		return setJournalModeWAL(ctx, conn, walRetryBudget)
	}
	return conn.execPragma(ctx, pragma)
}

// setJournalModeWAL switches the connection's database into WAL mode, waiting
// out a concurrent converter rather than failing on the first refusal.
//
// The wait has to be explicit because busy_timeout does not apply here.
// SQLite takes the journal-mode change's exclusive lock WITHOUT consulting the
// busy handler, so `PRAGMA journal_mode = WAL` returns SQLITE_BUSY in
// microseconds even on a connection whose busy_timeout is 5s — measured, and
// pinned by TestSetJournalModeWAL_BusyTimeoutAloneDoesNotCoverIt. Hoisting
// busy_timeout above this statement is necessary (it covers the migration that
// follows, and every later write) but on its own it is NOT sufficient.
//
// Losing the race is not an error condition: the winner leaves the database in
// WAL mode, which is exactly what this call wanted, so each attempt re-reads
// the mode first and returns as soon as it is "wal" whoever got it there. That
// read is also what keeps this cheap now that it runs on every connection
// (MGIT-121.1): WAL is a persistent database property, so after the first
// converter every later connection finds "wal" already set and returns without
// asking for the exclusive lock.
// Refs: MGIT-121.1, MGIT-121, MGIT-113, CLAUDE.md SQL Rule 2
func setJournalModeWAL(ctx context.Context, conn pragmaConn, budget time.Duration) error {
	// Real elapsed time, deliberately not the injected clock: this is a
	// backoff against other processes, and a frozen test clock would spin.
	deadline := time.Now().Add(budget)
	backoff := walRetryInitial
	var lastErr error

	for {
		mode, err := journalMode(ctx, conn)
		if err != nil {
			return err
		}
		if strings.EqualFold(mode, "wal") {
			return nil // already converted, by us or by whoever won
		}

		var got string
		got, lastErr = conn.queryPragma(ctx, walPragma)
		switch {
		case lastErr == nil && strings.EqualFold(got, "wal"):
			return nil
		case lastErr == nil:
			return fmt.Errorf("journal mode is %q after requesting WAL", got)
		case !isBusy(lastErr):
			return lastErr
		}

		if remaining := time.Until(deadline); remaining <= 0 {
			return fmt.Errorf("still locked after %s: %w", budget, lastErr)
		} else if backoff > remaining {
			backoff = remaining
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > walRetryMax {
			backoff = walRetryMax
		}
	}
}

// journalMode reads the database's current journal mode.
func journalMode(ctx context.Context, conn pragmaConn) (string, error) {
	// PRAGMA journal_mode (no assignment) reports the mode without locking.
	mode, err := conn.queryPragma(ctx, "PRAGMA journal_mode")
	if err != nil {
		return "", fmt.Errorf("read journal mode: %w", err)
	}
	return mode, nil
}

// isBusy reports whether err is SQLITE_BUSY (the lock is held elsewhere and
// the caller may retry), as opposed to a real failure.
func isBusy(err error) bool {
	var serr *sqlite.Error
	if errors.As(err, &serr) {
		return serr.Code()&0xff == sqliteBusy
	}
	return false
}
