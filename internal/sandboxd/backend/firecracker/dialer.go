package firecracker

import (
	"context"
	"net"

	"github.com/hyper-swe/mgit/internal/sandboxd/backend/microvm"
	"github.com/hyper-swe/mgit/internal/sandboxd/fcvsock"
)

// guestVsockPort is the guest-side vsock port the mgit-guest supervisor
// listens on (cmd/mgit-guest defaults --vsock-port to this). The host
// reaches the guest's exec/land channel at this port over the per-VM
// firecracker vsock socket. Refs: FR-17.11
const guestVsockPort = 1024

// guestDialer is the firecracker realization of microvm.GuestDialer: it
// maps a sandbox ID to that VM's per-VM vsock unix socket and dials the
// guest's vsock port through it (fcvsock's firecracker handshake). It is
// pure host-side I/O — no CGO, no KVM — so it is unit-testable against a
// fake firecracker socket; only a live guest listener is hardware-bound.
// Refs: FR-17.11, FR-17.16
type guestDialer struct {
	// workDir is microvm.Manager's sandbox state root; each sandbox's
	// artifacts (overlay, sockets) live under <workDir>/<sandbox-id>.
	workDir string
}

// newGuestDialer returns a dialer over the manager's sandbox state root.
func newGuestDialer(workDir string) *guestDialer {
	return &guestDialer{workDir: workDir}
}

// vsockSocketPath returns the firecracker per-VM vsock socket for a
// sandbox. The state dir comes from microvm.SandboxStateDir (the single
// source of the <workDir>/<sandbox-id> convention the manager creates
// under), and the socket name from the firecracker artifact layout
// (sandboxPaths); a drift in either breaks TestGuestDialer_PathMatches.
func (d *guestDialer) vsockSocketPath(sandboxID string) string {
	return sandboxPaths(microvm.SandboxStateDir(d.workDir, sandboxID)).vsock
}

// DialGuest connects to the bound guest's exec/land channel over its
// firecracker vsock socket. The returned conn speaks the execwire/land
// protocol once handed to guestexec.Run or the land orchestrator.
// Refs: FR-17.11
func (d *guestDialer) DialGuest(ctx context.Context, sandboxID string) (net.Conn, error) {
	return fcvsock.Dial(ctx, d.vsockSocketPath(sandboxID), guestVsockPort)
}
