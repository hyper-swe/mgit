// Package sandboxd implements the mgit-sandboxd helper daemon
// lifecycle: started on demand, supervising sandboxes over a local
// unix socket, idling at zero cost, and exiting once no sandboxes
// remain (NFR-17.6). Platform VMM backends plug in behind
// model.SandboxManager; any CGO lives in those backends, never in core
// mgit (FR-17.16, ADR-005 CGO containment).
// Refs: FR-17.16, FR-17.34, NFR-17.6, MGIT-11.4.1, MGIT-11.4.2
package sandboxd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/hyper-swe/mgit/internal/buildinfo"
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
	// Service dispatches authenticated control requests (launch/exec/
	// list/remove/status). When nil the daemon greets only — a backend
	// build without a wired service still authenticates and reports
	// liveness but serves no operations. Refs: MGIT-11.10.8
	Service SandboxDispatcher
	// Lander serves the land control-plane verb. It routes EXCLUSIVELY
	// through the verified LandOrchestrator (the daemon holds no persister,
	// importer, appender, or brancher): the only land capability the daemon
	// has is "land this task", which cannot import objects without
	// host-side verification. When nil, land is not served. Refs: MGIT-11.10.10, SEC-01
	Lander SandboxLander
	// Grants serves the capability-escalation control verbs (list pending
	// requests derived from observed denials; approve one into a live grant).
	// When nil those verbs report unserved (e.g. off Linux, where there is no
	// host egress runner to widen). Refs: FR-17.12, SEC-05
	Grants GrantCoordinator
	// Policy serves the live egress-policy verbs (replace a running
	// sandbox's allowlist without relaunching it; report the one in force).
	// When nil those verbs report unserved — never a silent success, which
	// for a revoke would leave the caller believing egress was closed.
	// Refs: MGIT-72, SEC-04
	Policy PolicyCoordinator
	// Exporter serves the guest->host artifact export verb (MGIT-73). When
	// nil the verb reports itself unserved — the honest answer for a backend
	// that cannot export rather than a silently weaker transfer. Refs: MGIT-73, ADR-011
	Exporter ArtifactExporter
	// HeartbeatInterval is how often the daemon writes a liveness beat on an
	// outstanding exec stream. Zero selects execwire.HeartbeatInterval — the
	// cadence the client judges silence against — and it is overridable only
	// so tests can compress it without waiting real seconds. Refs: MGIT-133
	HeartbeatInterval time.Duration
	// Watcher performs passive worktree observation on SnapshotInterval, so a
	// task's work is recoverable whether or not its agent checkpointed. When
	// nil, no observation runs and the daemon behaves exactly as before.
	// Refs: MGIT-110, R-H234
	Watcher WorktreeWatcher
	// SnapshotInterval is the passive observation cadence. It is deliberately
	// separate from PollInterval: the idle check is cheap and wants to be
	// frequent, while an observation walks the worktree and wants not to be.
	// Zero selects DefaultSnapshotInterval. Refs: MGIT-110
	SnapshotInterval time.Duration
	// MaxConns bounds concurrent in-flight connections; beyond it the
	// daemon rejects fast (accept-then-close) rather than spawning an
	// unbounded number of goroutines. Refs: MGIT-11.10.8 (security audit)
	MaxConns int
	// PeerBinder holds the sandbox->peer-identity bindings (the backend
	// Binds at launch and Invalidates at teardown). The daemon owns it to
	// authorize incoming guest->host land/attestation channels against the
	// addressed sandbox's binding (SEC-10); that accept path is wired in
	// MGIT-11.10.10. Refs: FR-17.27, SEC-10
	PeerBinder *PeerBinder
	// Version is the daemon's own build string (buildinfo.String()), stated in
	// the version handshake and named in a mismatch message. New fills it in
	// when it is empty; it is a Config field only so a test can pin it.
	// Refs: MGIT-136, MGIT-83
	Version string
	// PeerUID reads kernel-asserted peer credentials for one
	// connection. Nil selects the platform mechanism (SO_PEERCRED /
	// LOCAL_PEERCRED); injectable so foreign-UID rejection is testable
	// without root. Refs: FR-17.34
	PeerUID func(*net.UnixConn) (uint32, error)
}

// Defaults for lifecycle timing (overridable via Config).
const (
	defaultIdleGrace    = 30 * time.Second
	defaultPollInterval = time.Second
	// drainTimeout bounds shutdown: one hung backend must not stall the
	// daemon's exit indefinitely.
	drainTimeout = 30 * time.Second
	// defaultMaxConns bounds concurrent connections when Config.MaxConns is
	// unset — a backstop against goroutine exhaustion, well above the
	// single-client CLI's real concurrency. Refs: MGIT-11.10.8
	defaultMaxConns = 64
)

// greeting is the liveness line an authenticated peer receives; the
// activation health check keys off its "ok " prefix — a squatter
// socket that cannot greet is not the daemon (FR-17.34).
const greeting = "ok mgit-sandboxd\n"

// Daemon supervises sandboxes and owns the local IPC socket.
type Daemon struct {
	cfg     Config
	selfUID uint32 // daemon's own effective UID, cached at startup (FR-17.34)
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
	if cfg.Version == "" {
		cfg.Version = buildinfo.String()
	}
	if cfg.IdleGrace <= 0 {
		cfg.IdleGrace = defaultIdleGrace
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = defaultPollInterval
	}
	if cfg.PeerUID == nil {
		cfg.PeerUID = platformPeerUID
	}
	if cfg.MaxConns <= 0 {
		cfg.MaxConns = defaultMaxConns
	}
	return &Daemon{
		cfg:     cfg,
		selfUID: uint32(os.Geteuid()), //nolint:gosec // OK: Geteuid is non-negative on unix platforms where this runs
	}, nil
}

// Run serves the socket until the context is canceled (clean shutdown
// drains and destroys all sandboxes) or the daemon has had zero
// sandboxes — and no authenticated connections — for IdleGrace (idle
// exit: zero footprint when mgit is not in use, and unauthenticated
// peers cannot keep the daemon alive). The socket path is claimed via
// an exclusive flock so two daemons can never fight over it; stale
// files from crashed predecessors are replaced (restart safety).
// Refs: FR-17.16, FR-17.34, NFR-17.6
func (d *Daemon) Run(ctx context.Context) error {
	listener, lock, err := d.listen(ctx)
	if err != nil {
		return err
	}
	defer d.cleanupSocket(listener, lock)

	d.cfg.Logger.Info("sandboxd started", "event", "started", "socket", d.cfg.SocketPath)

	connections := make(chan net.Conn)
	acceptDone := make(chan error, 1)
	stop := make(chan struct{})
	defer close(stop)
	// authed receives one tick per AUTHENTICATED connection: only
	// legitimate peers reset the idle clock (an unauthorized dialer
	// must not control daemon lifetime).
	authed := make(chan struct{}, 16)
	// sem bounds concurrent connection handlers: a counted slot is taken
	// before each handler goroutine and released when it returns. At
	// capacity the daemon rejects fast rather than spawning unbounded
	// goroutines (security audit). Refs: MGIT-11.10.8
	sem := make(chan struct{}, d.cfg.MaxConns)
	go d.acceptLoop(listener, connections, acceptDone, stop)

	idleSince := d.cfg.Clock()
	ticker := time.NewTicker(d.cfg.PollInterval)
	defer ticker.Stop()

	// The passive observation cadence, separate from the idle poll above.
	snapTicker := d.newSnapshotTicker()
	defer snapTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			d.cfg.Logger.Info("sandboxd shutting down", "event", "shutdown", "reason", "signal")
			return d.drainBounded(ctx)

		case conn := <-connections:
			// Per-connection goroutine, bounded by the semaphore: a slow or
			// hung client must never block idle checks or shutdown, and a
			// flood must never exhaust goroutines (the daemon supervises
			// every VM). At capacity, reject fast.
			select {
			case sem <- struct{}{}:
				go func() {
					defer func() { <-sem }()
					d.handleConn(ctx, conn, authed)
				}()
			default:
				d.cfg.Logger.Warn("sandboxd at capacity, rejecting connection",
					"event", "conn_rejected", "max_conns", d.cfg.MaxConns)
				_ = conn.Close()
			}

		case <-authed:
			idleSince = d.cfg.Clock()

		case <-snapTicker.C:
			d.observeWorktrees(ctx)

		case err := <-acceptDone:
			// Accept failure (e.g. fd exhaustion) is fatal — but VMs are
			// NEVER left running unsupervised: drain before exiting.
			d.cfg.Logger.Error("sandboxd accept failed", "event", "accept_error", "error", err)
			return errors.Join(fmt.Errorf("sandboxd accept loop: %w", err), d.drainBounded(ctx))

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

// WorktreeWatcher performs one passive observation pass over the worktrees
// this daemon supervises, capturing any that have settled since the last one.
//
// It is an interface, and the daemon knows nothing about git: the daemon's job
// is to provide the CADENCE and to survive whatever the implementation does.
// Refs: MGIT-110, R-H234
type WorktreeWatcher interface {
	Observe(ctx context.Context) error
}

// DefaultSnapshotInterval is the passive observation cadence. It is long
// enough that walking a worktree is not a background cost worth noticing, and
// short enough that an interrupted run loses minutes rather than everything —
// which is the failure being fixed: thirty minutes of work that survived only
// as loose files. Refs: MGIT-110, MGIT-109
const DefaultSnapshotInterval = 90 * time.Second

// newSnapshotTicker returns the observation ticker, or a stopped one when no
// watcher is wired, so Run's select has a channel that simply never fires.
func (d *Daemon) newSnapshotTicker() *time.Ticker {
	interval := d.cfg.SnapshotInterval
	if interval <= 0 {
		interval = DefaultSnapshotInterval
	}
	t := time.NewTicker(interval)
	if d.cfg.Watcher == nil {
		t.Stop()
	}
	return t
}

// observeWorktrees runs one passive observation pass, converting any failure
// OR PANIC into a log line.
//
// Snapshotting is housekeeping; supervising microVMs is not. A watcher that
// fails must never stop the process holding the VMs, and the panic case is the
// same law MGIT-107 established for the drain: the daemon is the only thing
// that can reap those VMs, so it does not get to die over a background chore.
// Refs: MGIT-110, MGIT-107
func (d *Daemon) observeWorktrees(ctx context.Context) {
	if d.cfg.Watcher == nil {
		return
	}
	err := func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("panic observing worktrees: %v", r)
			}
		}()
		return d.cfg.Watcher.Observe(ctx)
	}()
	if err != nil {
		d.cfg.Logger.Error("sandboxd worktree observation failed", "event", "snapshot_error", "error", err)
	}
}

// drainBounded drains on a detached, bounded context: shutdown must
// finish even if the parent is canceled or a backend hangs.
func (d *Daemon) drainBounded(ctx context.Context) error {
	drainCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), drainTimeout)
	defer cancel()
	return d.drain(drainCtx)
}

// listen claims the socket path (exclusive flock — losers see "another
// daemon"), replaces any stale socket file, binds, and tightens the
// directory and socket modes (0700/0600, F-08). The flock closes the
// check-remove-rebind race: only the lock holder ever removes or binds
// the path.
func (d *Daemon) listen(ctx context.Context) (net.Listener, *socketLock, error) {
	if err := d.ensureSocketDir(); err != nil {
		return nil, nil, err
	}

	lock, err := acquireSocketLock(d.cfg.SocketPath)
	if err != nil {
		return nil, nil, fmt.Errorf("sandboxd: another daemon is serving %s: %w", d.cfg.SocketPath, err)
	}

	// We hold the claim: any existing file at the path is stale.
	if err := os.Remove(d.cfg.SocketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		lock.release()
		return nil, nil, fmt.Errorf("sandboxd: replace stale socket: %w", err)
	}

	var lc net.ListenConfig
	listener, err := lc.Listen(ctx, "unix", d.cfg.SocketPath)
	if err != nil {
		lock.release()
		return nil, nil, fmt.Errorf("sandboxd: bind %s: %w", d.cfg.SocketPath, err)
	}
	if err := os.Chmod(d.cfg.SocketPath, 0o600); err != nil {
		_ = listener.Close()
		lock.release()
		return nil, nil, fmt.Errorf("sandboxd: tighten socket mode: %w", err)
	}
	return listener, lock, nil
}

// ensureSocketDir creates (or tightens) the socket's parent directory
// to owner-only — sockets must never live in shared world-writable
// directories where squatting and symlink interposition are possible
// (FR-17.34). A directory we cannot chmod is not ours: fail closed.
func (d *Daemon) ensureSocketDir() error {
	dir := filepath.Dir(d.cfg.SocketPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("sandboxd: create socket dir: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // OK: 0700 is the minimum traversable owner-only DIRECTORY mode
		return fmt.Errorf("sandboxd: tighten socket dir (not owned by this user?): %w", err)
	}
	return nil
}

// acceptLoop feeds accepted connections to Run's select loop. The stop
// channel prevents a blocked hand-off from stranding a late client (or
// leaking this goroutine) when Run exits.
func (d *Daemon) acceptLoop(listener net.Listener, connections chan<- net.Conn, done chan<- error, stop <-chan struct{}) {
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
		select {
		case connections <- conn:
		case <-stop:
			_ = conn.Close()
			return
		}
	}
}

// handleConn services one IPC connection. Authentication comes FIRST:
// the kernel-asserted peer UID must equal the daemon's own UID before
// any byte of control protocol is processed — there is no
// unauthenticated path (F-08, ASVS V4). Authenticated peers tick the
// idle clock and receive the liveness greeting; the full request
// protocol arrives with the backends. Refs: FR-17.34
func (d *Daemon) handleConn(ctx context.Context, conn net.Conn, authed chan<- struct{}) {
	defer func() { _ = conn.Close() }()
	// A panic in one connection handler must NEVER crash the daemon: a hard
	// crash skips drain and strands every running VM unsupervised. Recover,
	// audit, and drop only this connection. Refs: MGIT-11.10.8 (security audit)
	defer func() {
		if r := recover(); r != nil {
			d.cfg.Logger.Error("sandboxd recovered from handler panic",
				"event", "handler_panic", "panic", fmt.Sprintf("%v", r))
		}
	}()

	if !d.authenticate(conn) {
		return // rejected and audited; nothing was processed
	}
	select {
	case authed <- struct{}{}:
	default: // Run is busy or exiting; the tick is best-effort.
	}
	// Liveness greeting for activation health checks; a write failure means
	// the peer is already gone, so there is nothing to serve.
	_ = conn.SetWriteDeadline(d.cfg.Clock().Add(time.Second))
	if _, err := conn.Write([]byte(greeting)); err != nil {
		return
	}
	d.serveHandshake(ctx, conn)
}

// authenticate verifies the peer's kernel-asserted UID matches the
// daemon's. Unverifiable peers fail closed. Every rejection is audited
// with the observed UID. Refs: FR-17.34
func (d *Daemon) authenticate(conn net.Conn) bool {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		d.cfg.Logger.Error("sandboxd rejected non-unix peer", "event", "auth_rejected", "reason", "not a unix socket")
		return false
	}
	peerUID, err := d.cfg.PeerUID(unixConn)
	if err != nil {
		d.cfg.Logger.Error("sandboxd rejected unverifiable peer", "event", "auth_rejected", "reason", "credential lookup failed", "error", err)
		return false
	}
	if peerUID != d.selfUID {
		d.cfg.Logger.Error("sandboxd rejected foreign peer", "event", "auth_rejected", "reason", "uid mismatch", "peer_uid", peerUID)
		return false
	}
	return true
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
	if d.cfg.Service != nil {
		return d.drainViaService(ctx)
	}
	return d.drainViaManager(ctx)
}

// drainViaService tears each sandbox down through the SERVICE, which is the
// layer that owns teardown semantics: it revokes capability grants, stops
// egress and published ports, stops and removes the VM, appends the terminal
// `destroyed` event, and drops the durable row last.
//
// It used to call the backend manager directly, and the terminal event lives
// one layer above the manager — so an orderly shutdown wrote no terminal event
// at all, and the NEXT daemon found a registration it could not verify and
// stamped it `killed / unsupervised`. That is the record a daemon CRASH is
// meant to produce. Since the daemon's own idle exit takes this path, the
// common case was manufacturing crash records for sandboxes that were torn
// down deliberately — diluting the signal a real crash should carry.
//
// The event is deliberately NOT written here as well. A second writer of the
// same terminal event is exactly how the two paths drifted apart. Refs: MGIT-107, MGIT-102, FR-17.19
func (d *Daemon) drainViaService(ctx context.Context) error {
	sandboxes, err := d.listForDrain(ctx)
	if err != nil {
		return fmt.Errorf("sandboxd drain: %w", err)
	}
	for _, sb := range sandboxes {
		// force: a shutdown cannot wait on a guest to exit gracefully.
		// Best-effort per sandbox — one that cannot be torn down must not
		// strand the rest, and it must not be recorded as cleanly destroyed
		// either: the service withholds the terminal event when the stop
		// fails, so the honest unsupervised record is what that one gets.
		if err := d.teardownForDrain(ctx, sb.TaskID); err != nil {
			d.cfg.Logger.Error("sandboxd drain teardown failed", "event", "drain_error",
				"sandbox_id", sb.ID, "task_id", sb.TaskID, "error", err)
			continue
		}
		d.cfg.Logger.Info("sandboxd drained sandbox", "event", "drained",
			"sandbox_id", sb.ID, "task_id", sb.TaskID)
	}
	return nil
}

// listForDrain and teardownForDrain call the service with a panic recovered
// into an error.
//
// The request-handling path already recovers per connection; the drain path
// did not, so a panic in one teardown killed the process MID-SHUTDOWN and
// every sandbox after it in the list was left running — orphaned VMs, the
// exact failure the lifeline and Pdeathsig work exists to prevent. Shutdown is
// the worst moment to lose the loop: there is no later pass. Refs: MGIT-107, FR-17.19
func (d *Daemon) listForDrain(ctx context.Context) (sandboxes []model.SandboxInfo, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic listing sandboxes to drain: %v", r)
		}
	}()
	return d.cfg.Service.List(ctx)
}

// teardownForDrain removes one sandbox through the service, converting a panic
// into an error so the loop continues to the next one. Refs: MGIT-107
func (d *Daemon) teardownForDrain(ctx context.Context, taskID string) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic tearing down task %q: %v", taskID, r)
		}
	}()
	// force: a shutdown cannot wait on a guest to exit gracefully.
	return d.cfg.Service.Remove(ctx, taskID, true)
}

// drainViaManager is the fallback for a daemon built without a wired service
// (backend-only: it greets and supervises but serves no operations —
// MGIT-11.10.8). Such a daemon still must not leave VMs running at shutdown,
// so it stops them through the backend. It cannot write a terminal event,
// because the layer that writes one is precisely what it does not have; that
// is stated in the log rather than left to look like a clean teardown.
// Refs: MGIT-107, MGIT-11.10.8
func (d *Daemon) drainViaManager(ctx context.Context) error {
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
		d.cfg.Logger.Info("sandboxd drained sandbox (no service wired: no terminal event recorded)",
			"event", "drained", "sandbox_id", sb.ID)
	}
	return nil
}

// cleanupSocket closes the listener, removes the socket file, and
// releases the path claim. The lock file itself is deliberately never
// unlinked (unlink-while-locked races a successor's open).
func (d *Daemon) cleanupSocket(listener net.Listener, lock *socketLock) {
	_ = listener.Close()
	if err := os.Remove(d.cfg.SocketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		d.cfg.Logger.Error("sandboxd socket cleanup failed", "event", "cleanup_error", "error", err)
	}
	lock.release()
}

// EnsureRunning is the activation door used by core mgit: if a daemon
// answers the socket WITH the authenticated greeting, nothing happens;
// otherwise spawn is invoked once and the socket awaited. A socket
// that accepts but cannot greet is a squatter, not the daemon.
// Concurrent callers racing a slow boot converge on the same daemon
// because the path claim is exclusive — a losing spawn exits with
// "another daemon is serving", which is benign. Refs: NFR-17.6, FR-17.34
func EnsureRunning(ctx context.Context, socketPath string, spawn func() error) error {
	return ensureRunning(ctx, socketPath, spawn, defaultActivationWait)
}

// defaultActivationWait bounds how long activation waits for a spawned
// daemon's socket to come up.
const defaultActivationWait = 5 * time.Second

// ensureRunning implements EnsureRunning with an injectable wait bound.
func ensureRunning(ctx context.Context, socketPath string, spawn func() error, wait time.Duration) error {
	// A canceled context means "stop", never "daemon is down — spawn".
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("sandboxd activation: %w", err)
	}
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

// dialOK reports whether a live, authenticated daemon serves the socket: the
// connection must yield the FULL liveness greeting, not merely accept and not
// merely emit a prefix. It compares the whole greeting exactly, symmetric with
// the real client (client.go dialGreeted), so a truncated or wrong greeting is
// treated as not-live rather than spuriously OK. Refs: FR-17.34
func dialOK(ctx context.Context, socketPath string) bool {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", socketPath)
	if err != nil {
		return false
	}
	defer func() { _ = conn.Close() }()

	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, len(greeting))
	if _, err := io.ReadFull(conn, buf); err != nil {
		return false
	}
	return string(buf) == greeting
}
