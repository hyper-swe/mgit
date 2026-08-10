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
	// TransparentPort is where the tap's REDIRECT sends the guest's ordinary
	// TCP, and where the transparent proxy listens. It is what lets an
	// unmodified guest program egress in allowlist mode at all. Refs: MGIT-69
	TransparentPort int
	// OriginalDst recovers the destination a redirected connection was
	// heading to before the kernel rewrote it. nil selects the platform's
	// real mechanism (Linux getsockopt SO_ORIGINAL_DST).
	//
	// It is a seam for the same reason Dial and Lookup are: the transparent
	// path is the one a real guest takes, and without a substitute for the
	// kernel's redirect it can only be exercised with root, a tap and
	// iptables — which is how it went untested long enough for its flows to
	// be silently untracked. Refs: MGIT-72, MGIT-69
	OriginalDst OriginalDstFunc
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
	// TransparentAddr is the redirect target an ordinary guest program's TCP
	// is steered to in allowlist mode (MGIT-69). Empty in none/open mode.
	TransparentAddr string
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
	// denialObserver, when set, is forwarded to each sandbox's authorizer so a
	// host-observed egress denial can be escalated to a capability request (the
	// deny->prompt trigger). Set once at daemon wiring (SetDenialObserver),
	// before any Start; guarded by mu. Refs: FR-17.12, SEC-05
	denialObserver func(model.ObservedDenial)
}

// activeEgress holds the running listeners for one sandbox plus its
// supervisor, so a host-approved capability grant can widen the LIVE
// allowlist (FR-17.12, SEC-05).
type activeEgress struct {
	cancel      context.CancelFunc
	tcp         net.Listener
	udp         net.PacketConn
	transparent net.Listener
	endpoints   Endpoints
	sup         *Supervisor
}

// closeListeners releases whatever sockets are open. It is the partial-failure
// path during Start (before cancel exists) and is safe on absent listeners:
// open mode has no TCP half at all.
func (a *activeEgress) closeListeners() {
	for _, l := range []net.Listener{a.tcp, a.transparent} {
		if l != nil {
			_ = l.Close()
		}
	}
	if a.udp != nil {
		_ = a.udp.Close()
	}
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

// SetDenialObserver installs the escalation observer notified of each
// host-observed egress denial, so denials can be turned into capability
// requests (the deny->prompt trigger, FR-17.12). Set once at daemon wiring,
// before any sandbox starts; it is forwarded to every per-sandbox authorizer.
// Refs: FR-17.12, SEC-05
func (r *Runner) SetDenialObserver(fn func(model.ObservedDenial)) {
	r.mu.Lock()
	r.denialObserver = fn
	r.mu.Unlock()
}

// Start brings up the egress stack for one allowlist-mode sandbox and
// returns where it is listening. For none/open modes it is a no-op (no
// proxy) returning empty endpoints. It is an error to start the same
// sandbox twice. Refs: SEC-04, FR-17.8
func (r *Runner) Start(ctx context.Context, b Binding) (Endpoints, error) {
	switch b.Policy.Mode {
	case model.NetworkModeNone:
		return Endpoints{}, nil // no NIC at all
	case model.NetworkModeOpen:
		// Open uses host NAT for its flows, so it runs no proxy — but it does
		// need a RESOLVER on the gateway, because that is where the guest is
		// told to look. It used to bind nothing there, so every name in open
		// mode failed against a dead port while the raw-IP tests stayed green.
		// Refs: MGIT-69, SEC-07
		return r.startOpenDNS(ctx, b)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.active[b.SandboxID]; exists {
		return Endpoints{}, fmt.Errorf("egress runner: sandbox %q already started", b.SandboxID)
	}

	sup, err := NewSupervisor(SupervisorConfig{
		SandboxID: b.SandboxID, TaskID: b.TaskID, Policy: b.Policy,
		Audit: r.cfg.Audit, Lookup: r.cfg.Lookup, Dial: r.cfg.Dial,
		Clock: r.cfg.Clock, Logger: r.cfg.Logger, OnDenial: r.denialObserver,
	})
	if err != nil {
		return Endpoints{}, fmt.Errorf("egress runner: %w", err)
	}

	ae, err := r.listen(ctx, b.GatewayIP)
	if err != nil {
		return Endpoints{}, err
	}
	ae.sup = sup
	// The TRANSPARENT listener is what an ordinary guest program actually
	// uses: the tap REDIRECTs its TCP here, and this proxy authorizes it on
	// the original destination. The length-prefixed CONNECT proxy above stays
	// for callers that speak it, but nothing in a guest does — which is why
	// allowlist mode was unusable from inside. Refs: MGIT-69, SEC-04
	transparent, err := NewTransparentProxy(TransparentProxyConfig{
		Authorizer: sup.Authorizer(), Dial: r.cfg.Dial, Logger: r.cfg.Logger,
		OriginalDst: r.cfg.OriginalDst,
		// TRACKED, and this is the line whose absence made live revoke a
		// half-truth on this backend: the CONNECT proxy above was registering
		// its flows while THIS path — the only one an ordinary guest program
		// takes — was not. A revoke therefore swapped the ruleset and
		// reported killed=0 with the guest's connection still carrying data.
		// Refs: MGIT-72, MGIT-69, ADR-012
		Flows: sup.Flows(),
	})
	if err != nil {
		ae.closeListeners()
		return Endpoints{}, fmt.Errorf("egress runner: %w", err)
	}
	tln, err := r.listenTransparent(ctx, b.GatewayIP)
	if err != nil {
		ae.closeListeners()
		return Endpoints{}, err
	}
	ae.transparent = tln
	ae.endpoints.TransparentAddr = tln.Addr().String()

	//nolint:gosec // G118: cancel is stored in ae.cancel and invoked by Stop — the egress lifecycle deliberately outlives Start
	runCtx, cancel := context.WithCancel(ctx)
	ae.cancel = cancel
	go func() { _ = sup.Proxy().Serve(runCtx, ae.tcp) }()
	go func() { _ = sup.DNS().ServeUDP(runCtx, ae.udp) }()
	go func() { _ = transparent.Serve(runCtx, ae.transparent) }()

	r.active[b.SandboxID] = ae
	r.cfg.Logger.Info("sandbox egress started", "event", "egress_started",
		"sandbox_id", b.SandboxID, "task_id", b.TaskID,
		"proxy", ae.endpoints.ProxyAddr, "dns", ae.endpoints.DNSAddr,
		"transparent", ae.endpoints.TransparentAddr)
	return ae.endpoints, nil
}

// listenTransparent binds the redirect target on the gateway.
func (r *Runner) listenTransparent(ctx context.Context, gw netip.Addr) (net.Listener, error) {
	var lc net.ListenConfig
	addr := fmt.Sprintf("%s:%d", gw, r.cfg.TransparentPort)
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("egress runner: listen transparent proxy %s: %w", addr, err)
	}
	return ln, nil
}

// startOpenDNS serves open mode's resolver on the sandbox gateway. It binds
// the UDP socket only: open mode's flows go out through host NAT, so there is
// no proxy to run, but name resolution still goes through mgit — which is
// what keeps it audited, rate-limited and subject to the unconditional IP
// denials rather than being whatever the guest reached through NAT.
// Refs: MGIT-69, SEC-04, SEC-07
func (r *Runner) startOpenDNS(ctx context.Context, b Binding) (Endpoints, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.active[b.SandboxID]; exists {
		return Endpoints{}, fmt.Errorf("egress runner: sandbox %q already started", b.SandboxID)
	}

	dns, err := NewOpenDNSServer(OpenDNSConfig{
		SandboxID: b.SandboxID, TaskID: b.TaskID,
		Audit: r.cfg.Audit, Lookup: r.cfg.Lookup, Clock: r.cfg.Clock, Logger: r.cfg.Logger,
	})
	if err != nil {
		return Endpoints{}, fmt.Errorf("egress runner: %w", err)
	}

	var lc net.ListenConfig
	udpAddr := fmt.Sprintf("%s:%d", b.GatewayIP, r.cfg.DNSPort)
	udp, err := lc.ListenPacket(ctx, "udp", udpAddr)
	if err != nil {
		return Endpoints{}, fmt.Errorf("egress runner: listen open-mode dns %s: %w", udpAddr, err)
	}

	//nolint:gosec // G118: cancel is stored in ae.cancel and invoked by Stop — the egress lifecycle deliberately outlives Start
	runCtx, cancel := context.WithCancel(ctx)
	ae := &activeEgress{
		cancel: cancel, udp: udp,
		endpoints: Endpoints{DNSAddr: udp.LocalAddr().String()},
	}
	go func() { _ = dns.ServeUDP(runCtx, udp) }()

	r.active[b.SandboxID] = ae
	r.cfg.Logger.Info("sandbox open-mode dns started", "event", "egress_open_dns_started",
		"sandbox_id", b.SandboxID, "task_id", b.TaskID, "dns", ae.endpoints.DNSAddr)
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
	// An open-mode sandbox has a DNS socket but NO proxy or transparent
	// listener, so the TCP halves are legitimately absent here.
	var err1 error
	if ae.tcp != nil {
		err1 = ae.tcp.Close()
	}
	if ae.transparent != nil {
		_ = ae.transparent.Close()
	}
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
