//go:build !linux && !darwin

package sandboxd

import (
	"errors"
	"fmt"
	"os"
)

// fileLock on platforms without flock falls back to O_EXCL marker-file
// semantics; the Windows daemon arrives with the Hyper-V backend
// (MGIT-11.5.3), which replaces this with a named-mutex claim. The daemon
// claims its socket path with one (FR-17.34) and, from MGIT-197 on, the host
// root whose index it serves. Refs: FR-17.34, MGIT-197
type fileLock struct {
	path string
}

// errLockHeld marks a claim another process holds.
var errLockHeld = errors.New("held by another process")

// acquireFileLock claims path via exclusive create.
func acquireFileLock(path string) (*fileLock, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // path is daemon config, not user input
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errLockHeld, err)
	}
	_ = file.Close()
	return &fileLock{path: path}, nil
}

// write records the holder's identity in the marker file. Refs: MGIT-197
func (l *fileLock) write(content string) error {
	return os.WriteFile(l.path, []byte(content), 0o600)
}

// release drops the claim by removing the marker file.
func (l *fileLock) release() {
	if l != nil && l.path != "" {
		_ = os.Remove(l.path)
	}
}

// acquireSocketLock claims <socketPath>.lock. Refs: FR-17.34
func acquireSocketLock(socketPath string) (*fileLock, error) {
	lock, err := acquireFileLock(socketPath + ".lock")
	if err != nil {
		return nil, fmt.Errorf("socket path is claimed: %w", err)
	}
	return lock, nil
}
