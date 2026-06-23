package sandboxd

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"time"
)

// NotifyLander runs the verified, host-initiated land for a task. It is the
// SAME capability the `mgit sandbox land` control verb routes through
// (service.LandService.Land, behind the daemon's SandboxLander seam): the
// notify channel is ONLY a trigger, so the entire land — pull, dual-hash
// verify, tree binding, require_sandbox, atomic import — stays host-side and
// unchanged. The trigger imports nothing itself (SEC-01 no-bypass).
// Refs: MGIT-11.10.11, SEC-01
type NotifyLander interface {
	Land(ctx context.Context, taskID string) (commits int, branch string, err error)
}

// NotifyTaskResolver maps a host-assigned sandbox ID to the task it is bound
// to. It is HOST-anchored: the bound task comes from the host's own sandbox
// registry, never from any guest-supplied text (SEC-05). *NotifyRegistry
// satisfies it. Refs: MGIT-11.10.11, SEC-05
type NotifyTaskResolver interface {
	// TaskFor returns the task bound to a sandbox, or ("", false) when the
	// sandbox is unknown (never launched or already torn down).
	TaskFor(sandboxID string) (taskID string, ok bool)
}

// notifyLandTimeout bounds one triggered land so a slow or stuck guest land
// pull cannot pin the per-VM accept goroutine indefinitely.
const notifyLandTimeout = 2 * time.Minute

// NotifyServer is the HOST side of the guest->host land-ready notification
// (the auto-land trigger). It listens on a PER-VM socket, so an inbound
// connection's only possible origin is that one VM (the firecracker
// guest->host model has the host listen on a per-VM "<vsock>_<port>" socket;
// the per-VM socket path is the host-observed identity). For each connection
// it AUTHORIZES the socket's host-observed peer identity against the addressed
// sandbox's launch binding BEFORE acting (SEC-10, fail closed on an unbound or
// torn-down sandbox, or a peer that does not match the binding), resolves the
// HOST-bound task for that sandbox (never trusting any guest text), and runs
// the verified host-initiated land. The connection carries NO land data and
// asserts NO provenance: the payload is ignored. Refs: MGIT-11.10.11, SEC-10, SEC-05, SEC-01
type NotifyServer struct {
	binder   *PeerBinder
	resolver NotifyTaskResolver
	lander   NotifyLander
	logger   *slog.Logger
}

// NewNotifyServer wires the notify server. All dependencies are required so a
// misconfiguration fails loudly rather than silently serving an unverified
// trigger.
func NewNotifyServer(binder *PeerBinder, resolver NotifyTaskResolver, lander NotifyLander, logger *slog.Logger) (*NotifyServer, error) {
	switch {
	case binder == nil:
		return nil, fmt.Errorf("notify server: peer binder must not be nil")
	case resolver == nil:
		return nil, fmt.Errorf("notify server: task resolver must not be nil")
	case lander == nil:
		return nil, fmt.Errorf("notify server: lander must not be nil")
	case logger == nil:
		return nil, fmt.Errorf("notify server: logger must not be nil")
	}
	return &NotifyServer{binder: binder, resolver: resolver, lander: lander, logger: logger}, nil
}

// Serve accepts guest->host notifications on one PER-VM listener until the
// context is canceled or the listener is closed (teardown). sandboxID is the
// host-assigned sandbox this listener belongs to; peerID is the host-observed
// peer identity for this VM's socket (the launch-bound identity the firecracker
// PeerIdentity reports — the per-VM vsock socket path). Because the listener is
// per-VM, a connection arriving here can ONLY be from this VM, and is
// authorized as (sandboxID, peerID); one guest can never reach another's
// listener. Each accepted connection is handled in its own goroutine so one
// slow trigger never blocks the next. Refs: MGIT-11.10.11, SEC-10
func (s *NotifyServer) Serve(ctx context.Context, sandboxID, peerID string, ln net.Listener) error {
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return nil // listener closed at teardown: the normal exit
			default:
				return fmt.Errorf("notify server: accept for sandbox %s: %w", sandboxID, err)
			}
		}
		go s.handle(ctx, sandboxID, peerID, conn)
	}
}

// handle authorizes one inbound notification and, if authorized, triggers the
// verified land for the sandbox's HOST-bound task. Every rejection is audited
// with host-observed identity only (SEC-05). The connection is drained-and-
// dropped: it carries no data the host trusts. Refs: MGIT-11.10.11, SEC-10, SEC-05, SEC-01
func (s *NotifyServer) handle(ctx context.Context, sandboxID, peerID string, conn net.Conn) {
	defer func() { _ = conn.Close() }()

	// Authorize the socket's host-observed peer against the addressed sandbox's
	// launch binding BEFORE acting. Fails closed on an unbound/torn-down
	// sandbox or a peer that does not match the binding (SEC-10): a guest must
	// not be able to trigger a land for a sandbox it is not the bound peer of.
	if err := s.binder.Authorize(sandboxID, peerID); err != nil {
		s.logger.Warn("notify trigger rejected: peer not authorized",
			"event", "notify_rejected", "sandbox_id", sandboxID, "reason", "peer authorization failed")
		return
	}

	// Resolve the HOST-bound task for this sandbox. The host never trusts the
	// guest for the task id; the binding comes from the host's own registry
	// (SEC-05). A sandbox with no bound task (raced teardown) fails closed.
	taskID, ok := s.resolver.TaskFor(sandboxID)
	if !ok {
		s.logger.Warn("notify trigger rejected: no bound task",
			"event", "notify_rejected", "sandbox_id", sandboxID, "reason", "sandbox has no host-bound task")
		return
	}

	// Run the EXISTING verified host-initiated land. The notify is purely a
	// trigger; all verification (require_sandbox + dual-hash + tree binding)
	// stays host-side in the orchestrator the lander routes through (SEC-01).
	landCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), notifyLandTimeout)
	defer cancel()
	commits, branch, err := s.lander.Land(landCtx, taskID)
	if err != nil {
		s.logger.Error("notify-triggered land failed",
			"event", "notify_land_error", "sandbox_id", sandboxID, "task_id", taskID, "error", err.Error())
		return
	}
	s.logger.Info("notify-triggered land completed",
		"event", "notify_landed", "sandbox_id", sandboxID, "task_id", taskID,
		"commits", commits, "branch", branch)
}
