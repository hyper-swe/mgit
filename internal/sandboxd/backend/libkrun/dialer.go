package libkrun

import (
	"context"
	"fmt"
	"net"

	"github.com/hyper-swe/mgit/internal/model"
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

// publishVsockPort converts a published GUEST TCP port into the vsock port
// number it is bridged on. They are deliberately the same number — mgit-guest
// runs an AF_VSOCK(:N)->TCP(127.0.0.1:N) bridge per published port, so a
// mapping would only create a way for the two ends to disagree. The range
// check is here rather than at the call site so every producer of a vsock
// port number goes through one validation. Refs: SEC-09, FR-17.8
func publishVsockPort(guestPort int) (uint32, error) {
	if guestPort < 1 || guestPort > 65535 {
		return 0, fmt.Errorf("%w: published guest port %d out of range",
			model.ErrSandboxBackendUnavailable, guestPort)
	}
	return uint32(guestPort), nil //nolint:gosec // OK: range-checked immediately above
}

// portDialer is the libkrun realization of microvm.GuestPortDialer: it dials
// an ARBITRARY published guest port over that port's own host unix socket.
//
// This — not netGateway.DialGuestPort — is how SEC-09 works on libkrun. The
// netstack gateway lives in the VM CHILD process, so the daemon holds no
// reference to it and cannot dial through it; libkrun's own listening vsock
// ports cross the process boundary by construction, which is also exactly the
// shape firecracker already uses. Host->guest only. Refs: SEC-09, FR-17.8
type portDialer struct {
	workDir string
}

// NewPortDialer returns a libkrun microvm.GuestPortDialer over the per-VM
// vsock port sockets under workDir. Refs: SEC-09
func NewPortDialer(workDir string) microvm.GuestPortDialer {
	return &portDialer{workDir: workDir}
}

// DialGuestPort connects to one published guest port on one sandbox. It fails
// closed when the port was never published (no libkrun listener behind it) so
// the host cannot reach anything the launch did not expose.
func (d *portDialer) DialGuestPort(ctx context.Context, sandboxID string, guestPort int) (net.Conn, error) {
	port, err := publishVsockPort(guestPort)
	if err != nil {
		return nil, err
	}
	path := vsockSocketPath(microvm.SandboxStateDir(d.workDir, sandboxID), port)
	var nd net.Dialer
	conn, err := nd.DialContext(ctx, "unix", path)
	if err != nil {
		return nil, fmt.Errorf("dial published guest port %d for sandbox %s: %w", guestPort, sandboxID, err)
	}
	return conn, nil
}
