package lock

import "time"

// Guarder runs a function while holding the process lock for the DURATION of
// that function only — acquiring a fresh lock and releasing it immediately
// after. It is the per-operation locking a long-lived server (`mgit serve`)
// uses so it never holds the exclusive repo lock for its lifetime and starve
// the CLI (MGIT-46). Because flock is per-open-file-description, a fresh
// Acquire per call also serializes concurrent in-process callers (two REST
// requests, or a REST and an MCP request), so the server needs no separate
// in-process write mutex.
//
// A CLI command still holds the lock for its (short) command lifetime via the
// App-level lock; only the server switches to per-operation guarding.
//
// Refs: MGIT-46, ADR-009
type Guarder struct {
	dir     string
	timeout time.Duration
}

// NewGuarder returns a Guarder that acquires the lock under mgitDir, waiting up
// to timeout for a contended lock.
func NewGuarder(mgitDir string, timeout time.Duration) *Guarder {
	return &Guarder{dir: mgitDir, timeout: timeout}
}

// Guard acquires the process lock, runs fn, then releases the lock — always,
// including when fn returns an error. A nil *Guarder is a pass-through: it runs
// fn without locking, for callers that already hold the lock for their lifetime
// (a CLI command) or unit tests with no locker wired. If the lock cannot be
// acquired within the timeout, fn does NOT run and the acquisition error is
// returned. Refs: MGIT-46
func (g *Guarder) Guard(fn func() error) error {
	if g == nil {
		return fn()
	}
	fl, err := Acquire(g.dir, g.timeout)
	if err != nil {
		return err
	}
	defer func() { _ = fl.Release() }()
	return fn()
}
