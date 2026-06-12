//go:build !linux && !darwin

package sandboxd

import (
	"fmt"
	"os"
)

// socketLock on platforms without flock falls back to O_EXCL pid-file
// semantics; the Windows daemon arrives with the Hyper-V backend
// (MGIT-11.5.3), which replaces this with a named-mutex claim.
type socketLock struct {
	path string
}

// acquireSocketLock claims <socketPath>.lock via exclusive create.
func acquireSocketLock(socketPath string) (*socketLock, error) {
	path := socketPath + ".lock"
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("socket path is claimed: %w", err)
	}
	_ = file.Close()
	return &socketLock{path: path}, nil
}

// release drops the claim by removing the marker file.
func (l *socketLock) release() {
	if l != nil && l.path != "" {
		_ = os.Remove(l.path)
	}
}
