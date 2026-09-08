package sandboxd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// hostLockName is the file inside a host root that names the daemon serving
// it. Refs: MGIT-197
const hostLockName = "daemon.lock"

// HostRootClaim is the exclusive claim on a host root's sandbox state — the
// index and everything beside it — held for a daemon's lifetime and released
// by the kernel if the daemon dies. It is taken BEFORE anything reads the
// index: a daemon that rehydrated first and claimed second discarded a live
// sandbox another daemon was serving, with a `killed` event that never
// happened (MGIT-197). Refs: MGIT-197
type HostRootClaim struct {
	hostRoot string
	lock     *fileLock
}

// ClaimHostRoot claims hostRoot for the daemon that will serve socket. An
// empty hostRoot claims nothing (greet-only daemons, tests) and returns nil.
// A root another live daemon holds is refused naming that daemon's pid and
// socket, read from what it wrote into the lock. Refs: MGIT-197
func ClaimHostRoot(hostRoot, socket string) (*HostRootClaim, error) {
	if hostRoot == "" {
		return nil, nil
	}
	path := filepath.Join(hostRoot, hostLockName)
	lock, err := acquireFileLock(path)
	if err != nil {
		if errors.Is(err, errLockHeld) {
			return nil, fmt.Errorf("sandboxd: another daemon already serves %s (%s); one repository has one daemon — "+
				"stop it with `mgit sandbox daemons stop --repo-root <its root>` or wait for its idle exit: %w",
				hostRoot, describeLockHolder(path), err)
		}
		return nil, fmt.Errorf("sandboxd: claim host root %s: %w", hostRoot, err)
	}
	if err := lock.write(fmt.Sprintf("pid %d\nsocket %s\n", os.Getpid(), socket)); err != nil {
		lock.release()
		return nil, fmt.Errorf("sandboxd: record the claim on %s: %w", hostRoot, err)
	}
	return &HostRootClaim{hostRoot: hostRoot, lock: lock}, nil
}

// Release drops the claim. Safe on a nil claim (nothing was claimed).
func (c *HostRootClaim) Release() {
	if c != nil {
		c.lock.release()
	}
}

// describeLockHolder reads what the holder wrote into the lock file, so the
// refusal can name it. Refs: MGIT-197
func describeLockHolder(path string) string {
	data, err := os.ReadFile(path) //nolint:gosec // a path this daemon derived from its own config
	text := strings.TrimSpace(string(data))
	if err != nil || text == "" {
		return "holder unknown: the lock file names nobody"
	}
	return strings.ReplaceAll(text, "\n", ", ")
}
