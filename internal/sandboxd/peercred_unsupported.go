//go:build !linux && !darwin

package sandboxd

import (
	"fmt"
	"net"
)

// platformPeerUID has no kernel peer-credential mechanism on this
// platform; authentication FAILS CLOSED — every connection is refused
// until a platform mechanism exists (Windows named-pipe SIDs arrive
// with the Hyper-V backend, MGIT-11.5.3). Refs: FR-17.34
func platformPeerUID(_ *net.UnixConn) (uint32, error) {
	return 0, fmt.Errorf("peer credentials: unsupported on this platform (fail closed)")
}
