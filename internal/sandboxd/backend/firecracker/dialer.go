package firecracker

import (
	"context"
	"net"

	"github.com/hyper-swe/mgit/internal/sandboxd/backend/microvm"
	"github.com/hyper-swe/mgit/internal/sandboxd/fcvsock"
)

// guestDialer is the firecracker realization of microvm.GuestDialer: it
// maps a sandbox ID to that VM's per-VM vsock unix socket and dials a
// guest vsock port through it (fcvsock's firecracker handshake). It is
// pure host-side I/O — no CGO, no KVM — so it is unit-testable against a
// fake firecracker socket; only a live guest listener is hardware-bound.
// The same type serves the exec channel (microvm.GuestExecPort) and,
// configured for microvm.GuestLandPort, the host land pull. The ports are
// single-sourced in the microvm package and shared with the vzf dialer and
// cmd/mgit-guest. Refs: FR-17.11, FR-17.5, FR-17.16
type guestDialer struct {
	// workDir is microvm.Manager's sandbox state root; each sandbox's
	// artifacts (overlay, sockets) live under <workDir>/<sandbox-id>.
	workDir string
	port    uint32 // guest vsock port to dial (exec or land)
}

// newGuestDialer returns a dialer for the guest exec channel.
func newGuestDialer(workDir string) *guestDialer {
	return &guestDialer{workDir: workDir, port: microvm.GuestExecPort}
}

// NewLandDialer returns a dialer for the guest LAND channel: it dials the
// guest's land port to pull the task branch's object pool. It is the
// concrete host land transport the daemon's land channel uses; it returns
// the microvm.GuestDialer contract (DialGuest) so the daemon depends on the
// transport interface, not a firecracker type. Refs: FR-17.5
func NewLandDialer(workDir string) microvm.GuestDialer {
	return &guestDialer{workDir: workDir, port: microvm.GuestLandPort}
}

// vsockSocketPath returns the firecracker per-VM vsock socket for a
// sandbox. The state dir comes from microvm.SandboxStateDir (the single
// source of the <workDir>/<sandbox-id> convention the manager creates
// under), and the socket name from the firecracker artifact layout
// (sandboxPaths); a drift in either breaks TestGuestDialer_PathMatches.
func (d *guestDialer) vsockSocketPath(sandboxID string) string {
	return sandboxPaths(microvm.SandboxStateDir(d.workDir, sandboxID)).vsock
}

// DialGuest connects to the bound guest's channel (exec or land, per the
// dialer's configured port) over its firecracker vsock socket. The returned
// conn speaks the execwire/land protocol once handed to guestexec.Run or
// the land channel. Refs: FR-17.11, FR-17.5
func (d *guestDialer) DialGuest(ctx context.Context, sandboxID string) (net.Conn, error) {
	return fcvsock.Dial(ctx, d.vsockSocketPath(sandboxID), d.port)
}
