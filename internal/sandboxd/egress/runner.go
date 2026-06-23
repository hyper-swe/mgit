package egress

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hyper-swe/mgit/internal/model"
)

// RunnerConfig wires the per-host egress runtime. Audit/Lookup/Dial/Clock/
// Logger are shared across all sandboxes; ProxyPort/DNSPort are the fixed
// gateway ports the tap firewall steers the guest to (0 => an ephemeral host
// port, for tests).
type RunnerConfig struct {
	Audit     Auditor
	Lookup    LookupFunc
	Dial      DialFunc
	Clock     func() time.Time
	Logger    *slog.Logger
	ProxyPort int
	DNSPort   int
}

// Binding describes one sandbox's egress: its identity, the host gateway IP
// of its tap, and its network policy. Refs: SEC-04, FR-17.8
type Binding struct {
	SandboxID string
	TaskID    string
	GatewayIP netip.Addr
	Policy    model.NetworkPolicy
}

// Endpoints reports where a started sandbox's egress proxy and DNS server
// are listening (so the firewall/guest config can point at them). Empty in
// none/open mode (no proxy). Refs: SEC-04
type Endpoints struct {
	ProxyAddr string
	DNSAddr   string
}

// Runner owns the lifecycle of the host egress stack for every running
// allowlist-mode sandbox: on Start it assembles the Supervisor and serves
// its proxy (TCP) and DNS (UDP) on the sandbox's gateway; on Stop it tears
// them down. none/open sandboxes run no proxy. This is the daemon's seam
// for wiring enforcement into the sandbox launch path. Refs: SEC-04, FR-17.8
type Runner struct {
	cfg RunnerConfig

	mu     sync.Mutex
	active map[string]*activeEgress
}

// activeEgress holds the running listeners for one sandbox plus its
// supervisor, so a host-approved capability grant can widen the LIVE
// allowlist (FR-17.12, SEC-05).
type activeEgress struct {
	cancel    context.CancelFunc
	tcp       net.Listener
	udp       net.PacketConn
	endpoints Endpoints
	sup       *Supervisor
}

// NewRunner validates the configuration and returns a Runner.
func NewRunner(cfg RunnerConfig) (*Runner, error) {
	switch {
	case cfg.Audit == nil:
		return nil, fmt.Errorf("egress runner: auditor must not be nil")
	case cfg.Lookup == nil:
		return nil, fmt.Errorf("egress runner: lookup must not be nil")
	case cfg.Dial == nil:
		return nil, fmt.Errorf("egress runner: dialer must not be nil")
	case cfg.Clock == nil:
		return nil, fmt.Errorf("egress runner: clock must not be nil")
	case cfg.Logger == nil:
		return nil, fmt.Errorf("egress runner: logger must not be nil")
	}
	return &Runner{cfg: cfg, active: make(map[string]*activeEgress)}, nil
}

// Start brings up the egress stack for one allowlist-mode sandbox and
// returns where it is listening. For none/open modes it is a no-op (no
// proxy) returning empty endpoints. It is an error to start the same
// sandbox twice. Refs: SEC-04, FR-17.8
func (r *Runner) Start(ctx context.Context, b Binding) (Endpoints, error) {
	if b.Policy.Mode != model.NetworkModeAllowlist {
		return Endpoints{}, nil // none has no NIC; open uses host NAT — no proxy
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.active[b.SandboxID]; exists {
		return Endpoints{}, fmt.Errorf("egress runner: sandbox %q already started", b.SandboxID)
	}

	sup, err := NewSupervisor(SupervisorConfig{
		SandboxID: b.SandboxID, TaskID: b.TaskID, Policy: b.Policy,
		Audit: r.cfg.Audit, Lookup: r.cfg.Lookup, Dial: r.cfg.Dial,
		Clock: r.cfg.Clock, Logger: r.cfg.Logger,
	})
	if err != nil {
		return Endpoints{}, fmt.Errorf("egress runner: %w", err)
	}

	ae, err := r.listen(ctx, b.GatewayIP)
	if err != nil {
		return Endpoints{}, err
	}
	ae.sup = sup
	//nolint:gosec // G118: cancel is stored in ae.cancel and invoked by Stop — the egress lifecycle deliberately outlives Start
	runCtx, cancel := context.WithCancel(ctx)
	ae.cancel = cancel
	go func() { _ = sup.Proxy().Serve(runCtx, ae.tcp) }()
	go func() { _ = sup.DNS().ServeUDP(runCtx, ae.udp) }()

	r.active[b.SandboxID] = ae
	r.cfg.Logger.Info("sandbox egress started", "event", "egress_started",
		"sandbox_id", b.SandboxID, "task_id", b.TaskID,
		"proxy", ae.endpoints.ProxyAddr, "dns", ae.endpoints.DNSAddr)
	return ae.endpoints, nil
}

// listen binds the proxy (TCP) and DNS (UDP) sockets on the gateway IP. On a
// partial failure it closes whatever opened so no socket leaks.
func (r *Runner) listen(ctx context.Context, gw netip.Addr) (*activeEgress, error) {
	var lc net.ListenConfig
	tcpAddr := fmt.Sprintf("%s:%d", gw, r.cfg.ProxyPort)
	tcp, err := lc.Listen(ctx, "tcp", tcpAddr)
	if err != nil {
		return nil, fmt.Errorf("egress runner: listen proxy %s: %w", tcpAddr, err)
	}
	udpAddr := fmt.Sprintf("%s:%d", gw, r.cfg.DNSPort)
	udp, err := lc.ListenPacket(ctx, "udp", udpAddr)
	if err != nil {
		_ = tcp.Close()
		return nil, fmt.Errorf("egress runner: listen dns %s: %w", udpAddr, err)
	}
	return &activeEgress{
		tcp: tcp, udp: udp,
		endpoints: Endpoints{ProxyAddr: tcp.Addr().String(), DNSAddr: udp.LocalAddr().String()},
	}, nil
}

// Stop tears down a sandbox's egress listeners. Stopping an unknown sandbox
// (none/open, or already torn down) is a no-op. Refs: FR-17.19
func (r *Runner) Stop(sandboxID string) error {
	r.mu.Lock()
	ae, ok := r.active[sandboxID]
	delete(r.active, sandboxID)
	r.mu.Unlock()
	if !ok {
		return nil
	}
	ae.cancel()
	err1 := ae.tcp.Close()
	err2 := ae.udp.Close()
	r.cfg.Logger.Info("sandbox egress stopped", "event", "egress_stopped", "sandbox_id", sandboxID)
	if err1 != nil {
		return fmt.Errorf("egress runner: close proxy: %w", err1)
	}
	if err2 != nil {
		return fmt.Errorf("egress runner: close dns: %w", err2)
	}
	return nil
}

// Running reports whether a sandbox's egress stack is currently active.
func (r *Runner) Running(sandboxID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.active[sandboxID]
	return ok
}

// AllowEgress applies a host-approved, sandbox-lifetime capability grant to a
// LIVE allowlist-mode sandbox: it widens that sandbox's running allowlist to
// admit the one host:port entry. The entry must be an exact IP:port (the grant
// names the host-observed destination, SEC-05) — a hostname/CIDR/wildcard is
// refused. Granting an unknown (or non-allowlist-mode, hence proxy-less)
// sandbox is an error (fail closed). This makes Runner satisfy the
// service-layer EgressGranter. Refs: FR-17.12, SEC-05
func (r *Runner) AllowEgress(_ context.Context, sandboxID, entry string) error {
	ip, port, err := parseGrantEntry(entry)
	if err != nil {
		return fmt.Errorf("egress runner: grant %q: %w", entry, err)
	}
	r.mu.Lock()
	ae, ok := r.active[sandboxID]
	r.mu.Unlock()
	if !ok {
		return fmt.Errorf("egress runner: grant: sandbox %q has no running egress stack", sandboxID)
	}
	if err := ae.sup.Allowlist().GrantIP(ip, port); err != nil {
		return fmt.Errorf("egress runner: grant: %w", err)
	}
	r.cfg.Logger.Info("sandbox egress grant applied", "event", "egress_grant",
		"sandbox_id", sandboxID, "dest", entry)
	return nil
}

// RevokeAll drops every live capability grant for a sandbox (teardown), so a
// grant never outlives its sandbox. An unknown sandbox is a no-op. Refs: FR-17.12, SEC-05
func (r *Runner) RevokeAll(sandboxID string) {
	r.mu.Lock()
	ae, ok := r.active[sandboxID]
	r.mu.Unlock()
	if ok {
		ae.sup.Allowlist().RevokeGrants()
	}
}

// allowlistFor returns a running sandbox's live allowlist (test seam for the
// grant path).
func (r *Runner) allowlistFor(sandboxID string) (*Allowlist, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ae, ok := r.active[sandboxID]
	if !ok {
		return nil, false
	}
	return ae.sup.Allowlist(), true
}

// parseGrantEntry parses an exact "ip:port" grant entry, rejecting hostnames,
// CIDRs, and wildcards — a grant names one host-observed destination (SEC-05).
func parseGrantEntry(entry string) (netip.Addr, int, error) {
	host, portStr, found := strings.Cut(entry, ":")
	if !found {
		return netip.Addr{}, 0, fmt.Errorf("must be ip:port")
	}
	// Rejoin for IPv6 (which contains its own colons): AddrPort parses both.
	if ap, err := netip.ParseAddrPort(entry); err == nil {
		return ap.Addr().Unmap(), int(ap.Port()), nil
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, 0, fmt.Errorf("host must be a literal IP")
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return netip.Addr{}, 0, fmt.Errorf("invalid port %q", portStr)
	}
	return ip.Unmap(), port, nil
}
