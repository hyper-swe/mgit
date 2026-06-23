package sandboxd

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
)

// NotifyController owns the PER-VM guest->host notify listeners (the auto-land
// trigger lifecycle). At launch it records the sandbox's HOST-bound task and
// opens a per-VM host listening socket, then serves notifications on it via the
// NotifyServer (authorize -> resolve host-bound task -> verified land). At
// teardown it closes that VM's listener (no host residue, FR-17.19) and drops
// the task binding so a recycled identity cannot inherit it (SEC-10). It is the
// host-anchored NotifyTaskResolver: TaskFor reads ONLY the host's own per-VM
// binding, never guest text (SEC-05). It is pure host I/O (no CGO, no KVM):
// fully unit-testable over an injected listener factory, and on Linux the real
// factory listens on the firecracker reverse-vsock per-VM socket.
// Refs: MGIT-11.10.11, SEC-10, SEC-05, FR-17.19
type NotifyController struct {
	server *NotifyServer
	listen ListenFunc
	logger *slog.Logger

	mu      sync.Mutex
	servers map[string]*notifyVM // sandbox ID -> live per-VM notify listener
	lander  NotifyLander         // late-bound: the verified land path, set after land wiring
}

// notifyVM holds one VM's notify binding and the handle to stop its listener.
type notifyVM struct {
	taskID string
	cancel context.CancelFunc
}

// ListenFunc opens a host listener for a sandbox's guest->host notify channel
// at the given socket path. It is injected so the controller is testable
// without a VM: the real factory (Linux) listens on the firecracker
// reverse-vsock per-VM unix socket; tests pass a unix-socket factory. The
// returned listener accepts inbound guest connections for exactly one VM.
type ListenFunc func(socketPath string) (net.Listener, error)

// NewNotifyController wires the controller. It owns its NotifyServer, wired to
// the controller as BOTH the host-anchored task resolver (TaskFor reads the
// per-VM binding recorded at Register) and the lander (forwarded to the
// late-bound verified land path, set via SetLander once land wiring completes).
// Until a lander is set, an authorized trigger fails closed rather than running
// an unverified land. binder, listen, and logger are required. Refs: MGIT-11.10.11
func NewNotifyController(binder *PeerBinder, listen ListenFunc, logger *slog.Logger) (*NotifyController, error) {
	switch {
	case binder == nil:
		return nil, fmt.Errorf("notify controller: peer binder must not be nil")
	case listen == nil:
		return nil, fmt.Errorf("notify controller: listen func must not be nil")
	case logger == nil:
		return nil, fmt.Errorf("notify controller: logger must not be nil")
	}
	c := &NotifyController{
		listen: listen, logger: logger,
		servers: make(map[string]*notifyVM),
	}
	srv, err := NewNotifyServer(binder, c, c, logger)
	if err != nil {
		return nil, err
	}
	c.server = srv
	return c, nil
}

// SetLander wires the late-bound verified land path. It is set once at daemon
// wiring time, after the land service is built, before any guest can boot and
// trigger. Until then an authorized trigger fails closed (no unverified land).
// Kept off the constructor because the land service is built after the manager
// the controller is handed to (a construction-order cycle). Refs: MGIT-11.10.11, SEC-01
func (c *NotifyController) SetLander(lander NotifyLander) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lander = lander
}

// Land forwards an authorized, host-resolved trigger to the late-bound verified
// land path. It fails closed when no lander is wired yet — the trigger NEVER
// imports objects itself (SEC-01). Refs: MGIT-11.10.11, SEC-01
func (c *NotifyController) Land(ctx context.Context, taskID string) (int, string, error) {
	c.mu.Lock()
	lander := c.lander
	c.mu.Unlock()
	if lander == nil {
		return 0, "", fmt.Errorf("notify controller: no verified land path wired")
	}
	return lander.Land(ctx, taskID)
}

// Register records a sandbox's host-bound task and starts serving its per-VM
// guest->host notify listener. peerID is the VM's host-observed peer identity
// (the launch-bound identity the NotifyServer authorizes against). socketPath
// is the per-VM host socket the guest reaches the host on. A listen failure is
// reported so the caller can decide (the auto-land trigger is best-effort; the
// host-initiated `mgit sandbox land` path is unaffected). Refs: MGIT-11.10.11, SEC-10
func (c *NotifyController) Register(sandboxID, taskID, peerID, socketPath string) error {
	ln, err := c.listen(socketPath)
	if err != nil {
		return fmt.Errorf("notify controller: listen for sandbox %s: %w", sandboxID, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	c.mu.Lock()
	// Replace any stale registration (relaunch reusing the ID): stop the old
	// listener first so two never serve the same sandbox.
	if old, ok := c.servers[sandboxID]; ok {
		old.cancel()
	}
	c.servers[sandboxID] = &notifyVM{taskID: taskID, cancel: cancel}
	c.mu.Unlock()

	go func() {
		// cancel is also called by Deregister; calling it here too guarantees the
		// context is released even if Serve returns on its own (no leak).
		defer cancel()
		if err := c.server.Serve(ctx, sandboxID, peerID, ln); err != nil {
			c.logger.Error("notify listener stopped with error",
				"event", "notify_serve_error", "sandbox_id", sandboxID, "error", err.Error())
		}
	}()
	c.logger.Info("notify listener started",
		"event", "notify_listening", "sandbox_id", sandboxID, "task_id", taskID)
	return nil
}

// Deregister stops a sandbox's notify listener and drops its task binding, so
// no listener outlives the VM and a recycled identity cannot inherit the
// binding (SEC-10, FR-17.19). Canceling the serve context closes the listener.
func (c *NotifyController) Deregister(sandboxID string) {
	c.mu.Lock()
	vm, ok := c.servers[sandboxID]
	delete(c.servers, sandboxID)
	c.mu.Unlock()
	if ok {
		vm.cancel()
		c.logger.Info("notify listener stopped",
			"event", "notify_stopped", "sandbox_id", sandboxID)
	}
}

// TaskFor returns the HOST-bound task for a sandbox, satisfying
// NotifyTaskResolver. It reads only the host's own per-VM binding recorded at
// Register, never any guest-supplied value (SEC-05). Refs: SEC-05
func (c *NotifyController) TaskFor(sandboxID string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	vm, ok := c.servers[sandboxID]
	if !ok {
		return "", false
	}
	return vm.taskID, true
}

// UnixListen is the default ListenFunc: it removes any stale socket file, binds
// a unix listener at socketPath, and tightens it to owner-only (0600). The
// firecracker guest->host reverse-vsock model has the host listen on a per-VM
// "<vsock>_<port>" unix socket the VMM forwards guest connections to; this is
// that listener. The path lives under the per-VM state dir, so teardown's one
// RemoveAll (and Deregister's listener close) leave no residue (FR-17.19).
// Refs: MGIT-11.10.11, FR-17.19
func UnixListen(socketPath string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o700); err != nil {
		return nil, fmt.Errorf("notify listen: create socket dir: %w", err)
	}
	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("notify listen: replace stale socket: %w", err)
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("notify listen: bind %s: %w", socketPath, err)
	}
	if err := os.Chmod(socketPath, 0o600); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("notify listen: tighten socket mode: %w", err)
	}
	return ln, nil
}
