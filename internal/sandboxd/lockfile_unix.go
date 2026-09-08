//go:build linux || darwin

package sandboxd

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// fileLock is an exclusive claim on a path, held for the daemon's lifetime
// via flock. The kernel releases it on process death, so a crashed daemon
// never wedges its successor. The daemon claims its socket path with one
// (FR-17.34) and, from MGIT-197 on, the host root whose index it serves.
// Refs: FR-17.34, MGIT-197
type fileLock struct {
	file *os.File
}

// errLockHeld marks a claim another live process holds.
var errLockHeld = errors.New("held by another process")

// acquireFileLock claims path exclusively, failing fast with errLockHeld if
// another live process holds it. The file is never unlinked (unlink-while-
// locked would race a successor's open).
func acquireFileLock(path string) (*fileLock, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // path is daemon config, not user input
	if err != nil {
		return nil, fmt.Errorf("open lock %s: %w", path, err)
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("%w: %w", errLockHeld, err)
	}
	return &fileLock{file: file}, nil
}

// write replaces the lock file's content with the holder's identity, so a
// process refused the claim can name who holds it. Refs: MGIT-197
func (l *fileLock) write(content string) error {
	if err := l.file.Truncate(0); err != nil {
		return err
	}
	if _, err := l.file.WriteAt([]byte(content), 0); err != nil {
		return err
	}
	return l.file.Sync()
}

// release drops the claim (closing the fd releases the flock).
func (l *fileLock) release() {
	if l != nil && l.file != nil {
		_ = l.file.Close()
	}
}

// acquireSocketLock claims <socketPath>.lock, the daemon's exclusive claim on
// its socket path. Refs: FR-17.34
func acquireSocketLock(socketPath string) (*fileLock, error) {
	lock, err := acquireFileLock(socketPath + ".lock")
	if err != nil {
		return nil, fmt.Errorf("socket path is claimed: %w", err)
	}
	return lock, nil
}
