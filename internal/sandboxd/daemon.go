// Package sandboxd implements the mgit-sandboxd helper daemon
// lifecycle: started on demand, supervising sandboxes over a local
// unix socket, idling at zero cost, and exiting once no sandboxes
// remain (NFR-17.6). Platform VMM backends plug in behind
// model.SandboxManager; any CGO lives in those backends, never in core
// mgit (FR-17.16, ADR-005 CGO containment). The IPC protocol and its
// same-UID peer authentication are layered on in MGIT-11.4.2.
// Refs: FR-17.16, NFR-17.6, MGIT-11.4.1
package sandboxd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"time"

	"github.com/hyper-swe/mgit/internal/model"
)

// Config wires the daemon's dependencies (DI everywhere; no globals).
type Config struct {
	SocketPath   string               // unix socket the daemon serves
	Manager      model.SandboxManager // supervised sandbox backend
	Logger       *slog.Logger         // structured logging (slog only)
	Clock        func() time.Time     // injected clock
	IdleGrace    time.Duration        // zero-sandbox linger before exit
	PollInterval time.Duration        // idle-check cadence
}

// Defaults for lifecycle timing (overridable via Config).
const (
	defaultIdleGrace    = 30 * time.Second
	defaultPollInterval = time.Second
	// drainTimeout bounds shutdown: one hung backend must not stall the
	// daemon's exit indefinitely.
	drainTimeout = 30 * time.Second
)

// Daemon supervises sandboxes and owns the local IPC socket.
type Daemon struct {
	cfg Config
}

// New validates the configuration and returns a Daemon.
func New(cfg Config) (*Daemon, error) {
	switch {
	case cfg.SocketPath == "":
		return nil, fmt.Errorf("sandboxd: socket path must not be empty")
	case cfg.Manager == nil:
		return nil, fmt.Errorf("sandboxd: sandbox manager must not be nil")
	case cfg.Logger == nil:
		return nil, fmt.Errorf("sandboxd: logger must not be nil")
	case cfg.Clock == nil:
		return nil, fmt.Errorf("sandboxd: clock must not be nil")
	}
	if cfg.IdleGrace <= 0 {
		cfg.IdleGrace = defaultIdleGrace
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = defaultPollInterval
	}
	return &Daemon{cfg: cfg}, nil
}

// Run serves the socket until the context is canceled (clean shutdown
// drains and destroys all sandboxes) or the daemon has had zero
// sandboxes for IdleGrace (idle exit — zero footprint when mgit is not
// in use). The socket file is removed on exit; a stale file from a
// crashed predecessor is replaced at startup (restart safety).
// Refs: FR-17.16, NFR-17.6
func (d *Daemon) Run(ctx context.Context) error {
	listener, err := d.listen(ctx)
	if err != nil {
		return err
	}
	defer d.cleanupSocket(listener)

	d.cfg.Logger.Info("sandboxd started", "event", "started", "socket", d.cfg.SocketPath)

	connections := make(chan net.Conn)
	acceptDone := make(chan error, 1)
	go d.acceptLoop(listener, connections, acceptDone)

	idleSince := d.cfg.Clock()
	ticker := time.NewTicker(d.cfg.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			d.cfg.Logger.Info("sandboxd shutting down", "event", "shutdown", "reason", "signal")
			// Drain on a detached, bounded context: shutdown must finish
			// even if the parent is canceled or a backend hangs.
			drainCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), drainTimeout)
			drainErr := d.drain(drainCtx)
			cancel()
			return drainErr

		case conn := <-connections:
			idleSince = d.cfg.Clock()
			// Per-connection goroutine: a slow or hung client must never
			// block idle checks or shutdown responsiveness.
			go d.handleConn(ctx, conn)

		case err := <-acceptDone:
			return fmt.Errorf("sandboxd accept loop: %w", err)

		case <-ticker.C:
			busy, err := d.hasSandboxes(ctx)
			if err != nil {
				d.cfg.Logger.Error("sandboxd list failed", "event", "list_error", "error", err)
				continue
			}
			if busy {
				idleSince = d.cfg.Clock()
				continue
			}
			if d.cfg.Clock().Sub(idleSince) >= d.cfg.IdleGrace {
				d.cfg.Logger.Info("sandboxd exiting idle", "event", "idle_exit",
					"idle_for", d.cfg.Clock().Sub(idleSince).String())
				return nil
			}
		}
	}
}

// listen binds the unix socket, replacing a stale file left behind by
// a crashed predecessor (restart safety).
func (d *Daemon) listen(ctx context.Context) (net.Listener, error) {
	var lc net.ListenConfig
	listener, err := lc.Listen(ctx, "unix", d.cfg.SocketPath)
	if err == nil {
		return listener, nil
	}

	// A live daemon answers dials; a stale file refuses them.
	if dialOK(ctx, d.cfg.SocketPath) {
		return nil, fmt.Errorf("sandboxd: another daemon is serving %s", d.cfg.SocketPath)
	}
	if rmErr := os.Remove(d.cfg.SocketPath); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
		return nil, fmt.Errorf("sandboxd: replace stale socket: %w", rmErr)
	}
	listener, err = lc.Listen(ctx, "unix", d.cfg.SocketPath)
	if err != nil {
		return nil, fmt.Errorf("sandboxd: bind %s: %w", d.cfg.SocketPath, err)
	}
	return listener, nil
}

// acceptLoop feeds accepted connections to Run's select loop.
func (d *Daemon) acceptLoop(listener net.Listener, connections chan<- net.Conn, done chan<- error) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			// Listener closed during shutdown is the normal exit path.
			if errors.Is(err, net.ErrClosed) {
				return
			}
			done <- err
			return
		}
		connections <- conn
	}
}

// handleConn services one IPC connection. The request protocol and its
// same-UID peer authentication land in MGIT-11.4.2; the lifecycle
// daemon only acknowledges liveness so activation can health-check.
func (d *Daemon) handleConn(_ context.Context, conn net.Conn) {
	defer func() { _ = conn.Close() }()
	d.cfg.Logger.Debug("sandboxd connection", "event", "connection")
}

// hasSandboxes reports whether any sandbox is registered.
func (d *Daemon) hasSandboxes(ctx context.Context) (bool, error) {
	sandboxes, err := d.cfg.Manager.List(ctx)
	if err != nil {
		return false, err
	}
	return len(sandboxes) > 0, nil
}

// drain stops and destroys every supervised sandbox (clean shutdown
// never leaves a VM running; landed work is untouched by design,
// FR-17.19). Errors are logged and the drain continues — one stuck
// sandbox must not strand the rest.
func (d *Daemon) drain(ctx context.Context) error {
	sandboxes, err := d.cfg.Manager.List(ctx)
	if err != nil {
		return fmt.Errorf("sandboxd drain: %w", err)
	}
	for _, sb := range sandboxes {
		if err := d.cfg.Manager.Stop(ctx, sb.ID, true); err != nil {
			d.cfg.Logger.Error("sandboxd drain stop failed", "event", "drain_error", "sandbox_id", sb.ID, "error", err)
		}
		if err := d.cfg.Manager.Remove(ctx, sb.ID, true); err != nil {
			d.cfg.Logger.Error("sandboxd drain remove failed", "event", "drain_error", "sandbox_id", sb.ID, "error", err)
			continue
		}
		d.cfg.Logger.Info("sandboxd drained sandbox", "event", "drained", "sandbox_id", sb.ID)
	}
	return nil
}

// cleanupSocket closes the listener and removes the socket file.
func (d *Daemon) cleanupSocket(listener net.Listener) {
	_ = listener.Close()
	if err := os.Remove(d.cfg.SocketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		d.cfg.Logger.Error("sandboxd socket cleanup failed", "event", "cleanup_error", "error", err)
	}
}

// EnsureRunning is the activation door used by core mgit: if the
// daemon's socket answers, nothing happens; otherwise spawn is invoked
// once and the socket awaited. Concurrent callers racing a slow boot
// converge on the same daemon because the socket bind is exclusive.
// Refs: NFR-17.6 (socket activation)
func EnsureRunning(ctx context.Context, socketPath string, spawn func() error) error {
	return ensureRunning(ctx, socketPath, spawn, defaultActivationWait)
}

// defaultActivationWait bounds how long activation waits for a spawned
// daemon's socket to come up.
const defaultActivationWait = 5 * time.Second

// ensureRunning implements EnsureRunning with an injectable wait bound.
func ensureRunning(ctx context.Context, socketPath string, spawn func() error, wait time.Duration) error {
	if dialOK(ctx, socketPath) {
		return nil
	}
	if err := spawn(); err != nil {
		return fmt.Errorf("sandboxd spawn: %w", err)
	}

	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("sandboxd activation: %w", err)
		}
		if dialOK(ctx, socketPath) {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("sandboxd activation: %s not dialable after spawn", socketPath)
}

// dialOK reports whether the daemon socket accepts connections.
func dialOK(ctx context.Context, socketPath string) bool {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", socketPath)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
