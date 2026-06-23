// Package portpub is the host-side controller for one-way guest->host port
// publishing (SEC-09). For each requested mapping it binds a 127.0.0.1 host
// listener (via egress.Publisher) and forwards every host-initiated
// connection INTO the guest over the per-VM GuestPortDialer. The direction
// is host->guest ONLY: there is no listener the guest could use to reach a
// host loopback service. It tracks the listeners per sandbox so teardown
// closes every one with no residue (FR-17.19). It is platform-agnostic host
// I/O (no CGO, no KVM) over the injected dialer, so it is wired wherever a
// GuestPortDialer exists and is fully unit-testable. Refs: SEC-09, FR-17.8, FR-17.19
package portpub

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/backend/microvm"
	"github.com/hyper-swe/mgit/internal/sandboxd/egress"
)

// Config wires the controller (DI; no globals).
type Config struct {
	// Dialer opens a connection to an arbitrary guest port on a sandbox
	// (the active backend's per-VM transport). Required.
	Dialer microvm.GuestPortDialer
	Logger *slog.Logger
}

// Controller manages every sandbox's published-port listeners. It satisfies
// service.PortPublishController. Refs: SEC-09
type Controller struct {
	dialer microvm.GuestPortDialer
	logger *slog.Logger

	mu     sync.Mutex
	active map[string][]net.Listener // sandbox ID -> open host loopback listeners
}

// New validates the configuration and returns a Controller.
func New(cfg Config) (*Controller, error) {
	switch {
	case cfg.Dialer == nil:
		return nil, fmt.Errorf("portpub: guest port dialer must not be nil")
	case cfg.Logger == nil:
		return nil, fmt.Errorf("portpub: logger must not be nil")
	}
	return &Controller{
		dialer: cfg.Dialer,
		logger: cfg.Logger,
		active: make(map[string][]net.Listener),
	}, nil
}

// StartPublish opens one 127.0.0.1 host listener per requested mapping and
// forwards each into the guest over the per-VM dialer (one-way, SEC-09). The
// ports are revalidated at the boundary (defense in depth) before any bind.
// A bind failure rolls back every listener already opened for this call (no
// half-published residue) and returns an error so the caller fails the boot
// closed. Refs: SEC-09, FR-17.8
func (c *Controller) StartPublish(ctx context.Context, info model.SandboxInfo, ports []model.PortPublish) error {
	if len(ports) == 0 {
		return nil
	}
	// Revalidate before binding: even though the model boundary already
	// rejected privileged/duplicate/out-of-range ports, treat the input as
	// hostile here too (the controller may be reached from another caller).
	for _, pp := range ports {
		if err := pp.Validate(); err != nil {
			return fmt.Errorf("portpub: sandbox %s: %w", info.ID, err)
		}
	}

	c.mu.Lock()
	if _, exists := c.active[info.ID]; exists {
		c.mu.Unlock()
		return fmt.Errorf("portpub: sandbox %s already has published ports", info.ID)
	}
	// Reserve the slot so a concurrent StartPublish for the same sandbox loses.
	c.active[info.ID] = nil
	c.mu.Unlock()

	listeners, err := c.openAll(ctx, info, ports)
	if err != nil {
		closeAll(listeners)
		c.mu.Lock()
		delete(c.active, info.ID)
		c.mu.Unlock()
		return err
	}

	c.mu.Lock()
	c.active[info.ID] = listeners
	c.mu.Unlock()
	c.logger.Info("sandbox ports published", "event", "ports_published",
		"sandbox_id", info.ID, "task_id", info.TaskID, "count", len(listeners))
	return nil
}

// openAll binds every requested port. One egress.Publisher is built PER
// mapping, each with a dialer that targets that mapping's guest port, so the
// host listen port (the Publish argument) and the guest dial port are
// independent. It returns the open listeners (the caller rolls them back on
// any error). Refs: SEC-09
func (c *Controller) openAll(ctx context.Context, info model.SandboxInfo, ports []model.PortPublish) ([]net.Listener, error) {
	listeners := make([]net.Listener, 0, len(ports))
	for _, pp := range ports {
		pub, err := egress.NewPublisher(egress.PublisherConfig{
			// The dialer targets THIS mapping's guest port for every host accept,
			// ignoring the publisher's listen-port argument — the host and guest
			// ports differ freely. Refs: SEC-09
			Dialer: &sandboxDialer{dialer: c.dialer, sandboxID: info.ID, guestPort: pp.GuestPort},
			Logger: c.logger,
		})
		if err != nil {
			return listeners, fmt.Errorf("portpub: sandbox %s: %w", info.ID, err)
		}
		// Publish binds 127.0.0.1:<HostPort> and forwards each host-initiated
		// connection into the guest's <GuestPort> (one-way). Refs: SEC-09
		ln, perr := pub.Publish(ctx, pp.HostPort)
		if perr != nil {
			return listeners, fmt.Errorf("portpub: sandbox %s: publish host %d -> guest %d: %w",
				info.ID, pp.HostPort, pp.GuestPort, perr)
		}
		listeners = append(listeners, ln)
	}
	return listeners, nil
}

// StopPublish closes every host listener for a sandbox so none outlives it
// (no residue, FR-17.19). Idempotent and safe to call for an unknown sandbox.
// Refs: SEC-09, FR-17.19
func (c *Controller) StopPublish(sandboxID string) {
	c.mu.Lock()
	listeners := c.active[sandboxID]
	delete(c.active, sandboxID)
	c.mu.Unlock()
	if len(listeners) == 0 {
		return
	}
	closeAll(listeners)
	c.logger.Info("sandbox ports unpublished", "event", "ports_unpublished",
		"sandbox_id", sandboxID, "count", len(listeners))
}

// closeAll closes every listener, ignoring already-closed errors.
func closeAll(listeners []net.Listener) {
	for _, ln := range listeners {
		if ln != nil {
			_ = ln.Close()
		}
	}
}

// HostAddrs returns the bound host loopback addresses for a sandbox's
// published ports. Test/diagnostic support: the listeners are owned by the
// controller. Refs: SEC-09
func (c *Controller) HostAddrs(sandboxID string) []net.Addr {
	c.mu.Lock()
	defer c.mu.Unlock()
	listeners := c.active[sandboxID]
	addrs := make([]net.Addr, 0, len(listeners))
	for _, ln := range listeners {
		addrs = append(addrs, ln.Addr())
	}
	return addrs
}

// HasSandbox reports whether the controller holds any live listeners for a
// sandbox. Test/diagnostic support. Refs: FR-17.19
func (c *Controller) HasSandbox(sandboxID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.active[sandboxID]
	return ok
}

// sandboxDialer adapts a per-VM microvm.GuestPortDialer to the
// egress.GuestPortDialer the publisher uses. It binds the sandbox ID and the
// target guest port, IGNORING the publisher's own port argument — that
// argument is the host listener port, not the guest port. This keeps host and
// guest ports independent. The host->guest direction only. Refs: SEC-09
type sandboxDialer struct {
	dialer    microvm.GuestPortDialer
	sandboxID string
	guestPort int
}

// DialGuestPort dials this sandbox's bound guest port over the backend
// transport (host->guest). The publisher's listen-port argument is ignored.
func (d *sandboxDialer) DialGuestPort(ctx context.Context, _ int) (net.Conn, error) {
	return d.dialer.DialGuestPort(ctx, d.sandboxID, d.guestPort)
}
