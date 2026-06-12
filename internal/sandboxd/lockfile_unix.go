//go:build linux || darwin

package sandboxd

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// socketLock is an exclusive claim on a socket path, held for the
// daemon's lifetime via flock. The kernel releases it on process death,
// so a crashed daemon never wedges its successor. Refs: FR-17.34
type socketLock struct {
	file *os.File
}

// acquireSocketLock claims <socketPath>.lock exclusively, failing fast
// if another live daemon holds it. The lock file is never unlinked
// (unlink-while-locked would race a successor's open).
func acquireSocketLock(socketPath string) (*socketLock, error) {
	file, err := os.OpenFile(socketPath+".lock", os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // path is daemon config, not user input
	if err != nil {
		return nil, fmt.Errorf("open socket lock: %w", err)
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("socket path is claimed: %w", err)
	}
	return &socketLock{file: file}, nil
}

// release drops the claim (closing the fd releases the flock).
func (l *socketLock) release() {
	if l != nil && l.file != nil {
		_ = l.file.Close()
	}
}
