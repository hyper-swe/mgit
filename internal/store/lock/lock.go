// Package lock provides process-level file locking for mgit.
//
// Every CLI command that opens stores must acquire this lock first
// to prevent races between concurrent mgit processes operating on
// the same repository.
//
// Lockfile location: .mgit/locks/mgit.lock
// Lockfile contents: PID of the holder (for diagnostics only)
// Acquisition: OS-level advisory lock (flock on Unix, LockFileEx on Windows)
//
// The lock is automatically released by the kernel when the process exits,
// even on SIGKILL, so a stale lockfile cannot block future runs.
//
// Refs: NFR-3 (reliability), MGIT-10
package lock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// DefaultTimeout is the maximum time a process will wait for the lock.
const DefaultTimeout = 30 * time.Second

// PollInterval is how often the waiter re-checks the lock.
const PollInterval = 50 * time.Millisecond

// ErrLockHeld indicates another process currently holds the lock.
var ErrLockHeld = errors.New("another mgit process is running")

// FileLock represents an acquired process-level lock.
type FileLock struct {
	path string
	file *os.File
}

// Acquire attempts to acquire the lock, blocking up to timeout.
// If the lock is held by another process, it polls until the holder
// releases it or the timeout expires.
//
// Security: the lockfile is opened with O_NOFOLLOW (where supported) to
// prevent symlink attacks against an attacker-controlled .mgit directory.
//
// Refs: NFR-5 (security), NFR-3 (reliability), MGIT-10.1
func Acquire(mgitDir string, timeout time.Duration) (*FileLock, error) {
	locksDir := filepath.Clean(filepath.Join(mgitDir, "locks"))
	if err := os.MkdirAll(locksDir, 0o700); err != nil {
		return nil, fmt.Errorf("create locks dir: %w", err)
	}

	lockPath := filepath.Clean(filepath.Join(locksDir, "mgit.lock"))

	// Reject the lockfile if it is a symlink (symlink attack defense).
	// Use Lstat which does not follow symlinks.
	if info, err := os.Lstat(lockPath); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("lockfile %q is a symlink, refusing to use", lockPath)
		}
	}

	// Open or create the lockfile with O_NOFOLLOW to prevent TOCTOU symlink races.
	// 0o600 = owner read+write only.
	flags := os.O_RDWR | os.O_CREATE | osNoFollow
	f, err := os.OpenFile(lockPath, flags, 0o600) //nolint:gosec // path is internal, derived from mgitDir; symlinks rejected above
	if err != nil {
		return nil, fmt.Errorf("open lockfile: %w", err)
	}

	deadline := time.Now().Add(timeout)
	for {
		err = lockFD(f)
		if err == nil {
			// Got the lock. Write our PID for diagnostics.
			if writeErr := writePID(f); writeErr != nil {
				_ = unlockFD(f)
				_ = f.Close()
				return nil, writeErr
			}
			return &FileLock{path: lockPath, file: f}, nil
		}

		if time.Now().After(deadline) {
			_ = f.Close()
			holderPID, holderCmd := readHolder(lockPath)
			if holderPID != 0 && holderCmd != "" {
				return nil, fmt.Errorf("%w: held by PID %d (%s) after %s",
					ErrLockHeld, holderPID, holderCmd, timeout)
			}
			if holderPID != 0 {
				return nil, fmt.Errorf("%w: held by PID %d after %s",
					ErrLockHeld, holderPID, timeout)
			}
			return nil, fmt.Errorf("%w: timeout after %s", ErrLockHeld, timeout)
		}
		time.Sleep(PollInterval)
	}
}

// Release releases the lock and closes the lockfile.
// It is safe to call multiple times; subsequent calls are no-ops.
func (l *FileLock) Release() error {
	if l.file == nil {
		return nil
	}
	_ = unlockFD(l.file)
	err := l.file.Close()
	l.file = nil
	return err
}

// Path returns the filesystem path of the lockfile.
func (l *FileLock) Path() string {
	return l.path
}

// writePID writes the holder's identity to the lockfile: the PID on the first
// line and a short command label on the second (e.g. "mgit serve"), so a
// contended waiter can name WHICH command holds the lock, not just its PID
// (MGIT-46). The format is backward compatible — readers parse the first line
// as the PID, so an older bare-PID lockfile still reads correctly.
func writePID(f *os.File) error {
	if err := f.Truncate(0); err != nil {
		return fmt.Errorf("truncate lockfile: %w", err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		return fmt.Errorf("seek lockfile: %w", err)
	}
	if _, err := fmt.Fprintf(f, "%d\n%s", os.Getpid(), holderLabel()); err != nil {
		return fmt.Errorf("write pid: %w", err)
	}
	return nil
}

// holderLabel is a concise, human-readable description of the current process's
// command (e.g. "mgit serve"), used only for diagnostics in a contended-lock
// error.
func holderLabel() string {
	return holderLabelFrom(os.Args)
}

// holderLabelFrom builds the label from an argv. Split out from holderLabel so
// the edge cases (empty argv, over-long argv, embedded newlines) are testable
// without mutating the os.Args global. It is capped so a pathological argv
// cannot bloat the lockfile, and kept single-line so the first-line-is-PID
// contract holds.
func holderLabelFrom(args []string) string {
	if len(args) == 0 {
		return ""
	}
	label := filepath.Base(args[0])
	if len(args) > 1 {
		label += " " + strings.Join(args[1:], " ")
	}
	const maxLabel = 120
	if len(label) > maxLabel {
		label = label[:maxLabel]
	}
	return strings.ReplaceAll(label, "\n", " ")
}

// readHolder reads the holder PID (first line) and command label (second line,
// if any) from an existing lockfile (best effort). Refs: MGIT-46
func readHolder(path string) (pid int, cmd string) {
	data, err := os.ReadFile(path) //nolint:gosec // internal lockfile
	if err != nil {
		return 0, ""
	}
	lines := strings.SplitN(string(data), "\n", 2)
	pid, _ = strconv.Atoi(strings.TrimSpace(lines[0]))
	if len(lines) > 1 {
		cmd = strings.TrimSpace(lines[1])
	}
	return pid, cmd
}

// readPID reads the PID from an existing lockfile (best effort).
func readPID(path string) int {
	pid, _ := readHolder(path)
	return pid
}
