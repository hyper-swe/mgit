//go:build linux

package sandboxd

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// platformPeerUID reads the connecting peer's UID via SO_PEERCRED —
// kernel-asserted credentials the client cannot forge (F-08, ASVS V4).
// Refs: FR-17.34
func platformPeerUID(conn *net.UnixConn) (uint32, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, fmt.Errorf("peer credentials: %w", err)
	}

	var cred *unix.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return 0, fmt.Errorf("peer credentials: %w", err)
	}
	if credErr != nil {
		return 0, fmt.Errorf("peer credentials: %w", credErr)
	}
	return cred.Uid, nil
}
