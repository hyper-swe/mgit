package libkrun

import (
	"context"
	"fmt"
	"net"

	"github.com/hyper-swe/mgit/internal/sandboxd/backend/microvm"
)

// guestDialer is the libkrun realization of microvm.GuestDialer. libkrun
// maps each guest vsock port to its OWN host unix socket
// (krun_add_vsock_port2), created by the VM child process and listened on by
// libkrun itself — so unlike firecracker there is no in-band "CONNECT
// <port>" handshake: the daemon simply dials the per-port socket and is
// through to the guest listener. Pure host-side I/O; the child process
// boundary is invisible here, which is what keeps the exec/land wiring
// backend-agnostic. Refs: FR-17.11, FR-17.5, FR-17.16, ADR-010
type guestDialer struct {
	workDir string // microvm.Manager's state root; sockets live under <workDir>/<sandbox-id>
	port    uint32 // guest vsock port to dial (exec or land)
}

// newGuestDialer returns a dialer for the guest exec channel.
func newGuestDialer(workDir string) *guestDialer {
	return &guestDialer{workDir: workDir, port: microvm.GuestExecPort}
}

// NewLandDialer returns a dialer for the guest LAND channel, satisfying the
// microvm.GuestDialer contract the daemon land wiring consumes. Refs: FR-17.5
func NewLandDialer(workDir string) microvm.GuestDialer {
	return &guestDialer{workDir: workDir, port: microvm.GuestLandPort}
}

// DialGuest connects to this dialer's guest vsock port on one sandbox.
func (d *guestDialer) DialGuest(ctx context.Context, sandboxID string) (net.Conn, error) {
	path := vsockSocketPath(microvm.SandboxStateDir(d.workDir, sandboxID), d.port)
	var nd net.Dialer
	conn, err := nd.DialContext(ctx, "unix", path)
	if err != nil {
		return nil, fmt.Errorf("dial libkrun vsock port %d for sandbox %s: %w", d.port, sandboxID, err)
	}
	return conn, nil
}
