package vzf

import (
	"context"
	"fmt"
	"net"
	"sync"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/backend/microvm"
)

// guestConnector opens a host->guest vsock connection to a port on one
// live VM. *vzVM implements it over the framework (the running
// VZVirtualMachine's VZVirtioSocketDevice.Connect); tests supply a fake.
// It is the seam that keeps the sandbox->VM resolution and the fail-closed
// policy (guestDialer, liveVMs) CGO-free and unit-testable, with only the
// framework Connect itself behind the darwin+cgo build tag. Refs: FR-17.16
type guestConnector interface {
	connectGuest(port uint32) (net.Conn, error)
}

// liveVMs is the vzf backend's registry of running VM handles keyed by
// sandbox ID. Unlike firecracker — which exposes vsock as a per-VM unix
// socket, so its dialer is stateless and reconstructs a path from the
// sandbox ID — vzf connects through the framework API on the live
// VZVirtualMachine, so the host dialer must resolve a sandbox ID to its
// live handle. The platform hypervisor registers a VM once it has started
// and drops it on stop; the dialer reads it. Concurrency-safe: the manager
// launches and tears down sandboxes from many goroutines. Refs: FR-17.16
type liveVMs struct {
	mu sync.RWMutex
	m  map[string]guestConnector
}

// newLiveVMs returns an empty registry.
func newLiveVMs() *liveVMs {
	return &liveVMs{m: make(map[string]guestConnector)}
}

// put registers a sandbox's live VM handle.
func (r *liveVMs) put(sandboxID string, c guestConnector) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.m[sandboxID] = c
}

// remove drops a sandbox's handle at teardown so a stale ID cannot reach a
// successor channel (SEC-10).
func (r *liveVMs) remove(sandboxID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.m, sandboxID)
}

// get resolves a sandbox to its live VM handle.
func (r *liveVMs) get(sandboxID string) (guestConnector, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	c, ok := r.m[sandboxID]
	return c, ok
}

// guestDialer is the vzf realization of microvm.GuestDialer: it resolves a
// sandbox ID to its live VM handle and opens a connection to a guest vsock
// port through the framework. It fails closed when the sandbox has no live
// VM, and the framework supplies the error when the VM has no socket
// device. The same type serves the exec channel (microvm.GuestExecPort)
// and, configured for the land port, the host land pull. Refs: FR-17.11, FR-17.16
type guestDialer struct {
	reg  *liveVMs
	port uint32
}

// newGuestExecDialer returns a dialer for the guest exec channel.
func newGuestExecDialer(reg *liveVMs) *guestDialer {
	return &guestDialer{reg: reg, port: microvm.GuestExecPort}
}

// DialGuest connects to the bound guest's channel over the live VM's vsock
// device. It fails closed (ErrSandboxBackendUnavailable) when no live VM is
// registered for the sandbox. The framework dial is synchronous and does
// not observe ctx; microvm.Manager.Exec applies the request deadline to the
// returned conn. Refs: FR-17.11, FR-17.16
func (d *guestDialer) DialGuest(_ context.Context, sandboxID string) (net.Conn, error) {
	c, ok := d.reg.get(sandboxID)
	if !ok {
		return nil, fmt.Errorf("%w: no live vzf VM for sandbox %q", model.ErrSandboxBackendUnavailable, sandboxID)
	}
	return c.connectGuest(d.port)
}
