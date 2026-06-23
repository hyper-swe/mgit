package egress

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"sync"
	"time"

	"github.com/hyper-swe/mgit/internal/model"
)

// Auditor appends one egress decision to the append-only audit log
// (satisfied by internal/store/index.Store.AppendEgressRecord). Every
// allow and every deny — DNS and TCP — is recorded (FR-17.8). Refs: FR-17.8
type Auditor interface {
	AppendEgressRecord(ctx context.Context, rec *model.EgressRecord) error
}

// LookupFunc resolves a hostname to IPs on the HOST (the guest has no
// resolver of its own). Injected so tests need no network and production
// can map a not-found result to ErrNXDOMAIN. Refs: SEC-07
type LookupFunc func(ctx context.Context, name string) ([]netip.Addr, error)

// ResolverConfig wires the host-side restricted resolver. All fields are
// required except the tuning knobs, which default to safe values.
type ResolverConfig struct {
	SandboxID string
	TaskID    string
	Allowlist *Allowlist
	Lookup    LookupFunc
	Audit     Auditor
	Clock     func() time.Time
	// Logger records audit-write failures so a dropped durable record is never
	// silent (CLAUDE.md "no swallowed errors"). Optional; nil => discard.
	Logger *slog.Logger
	// MaxQueriesPerWindow caps DNS queries per Window (0 => default 60).
	MaxQueriesPerWindow int
	// Window is the rate-limit window (0 => default one minute).
	Window time.Duration
	// NXDOMAINBurstThreshold flags label-enumeration / tunneling when this
	// many NXDOMAINs land in one window (0 => default 10).
	NXDOMAINBurstThreshold int
}

// Resolver resolves only allowlisted names, host-side, rate-limited, with
// NXDOMAIN-burst flagging — the SEC-07 anti-tunnel control. A successful
// resolution PINS the returned IPs: the egress proxy connects to exactly
// those bytes, never re-resolving, so a name cannot be rebound to a denied
// IP between resolution and connect (DNS-rebind defense). Refs: SEC-07, SEC-04
type Resolver struct {
	cfg    ResolverConfig
	logger *slog.Logger
	maxQPW int
	window time.Duration
	nxCap  int

	mu          sync.Mutex
	windowStart time.Time
	queryCount  int
	nxCount     int
	nxFlagged   bool
	pins        map[string][]netip.Addr
}

// NewResolver validates the configuration and returns a Resolver.
func NewResolver(cfg ResolverConfig) (*Resolver, error) {
	switch {
	case cfg.Allowlist == nil:
		return nil, fmt.Errorf("egress resolver: allowlist must not be nil")
	case cfg.Lookup == nil:
		return nil, fmt.Errorf("egress resolver: lookup must not be nil")
	case cfg.Audit == nil:
		return nil, fmt.Errorf("egress resolver: auditor must not be nil")
	case cfg.Clock == nil:
		return nil, fmt.Errorf("egress resolver: clock must not be nil")
	case cfg.SandboxID == "":
		return nil, fmt.Errorf("egress resolver: sandbox id must not be empty")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	r := &Resolver{
		cfg:    cfg,
		logger: logger,
		maxQPW: orDefault(cfg.MaxQueriesPerWindow, 60),
		window: cfg.Window,
		nxCap:  orDefault(cfg.NXDOMAINBurstThreshold, 10),
		pins:   make(map[string][]netip.Addr),
	}
	if r.window <= 0 {
		r.window = time.Minute
	}
	return r, nil
}

// Resolve returns the pinned IPs for an allowlisted name. It refuses
// non-allowlisted names without consulting the upstream resolver (no label
// exfiltration), enforces the per-window query cap, and counts NXDOMAIN
// bursts. Every decision is audited as a dns record. Refs: SEC-07, FR-17.8
func (r *Resolver) Resolve(ctx context.Context, name string) ([]netip.Addr, error) {
	if !r.cfg.Allowlist.HasName(name) {
		r.audit(ctx, model.EgressDeny, name, "dns: name not allowlisted")
		return nil, fmt.Errorf("%w: %q", ErrNameNotAllowlisted, name)
	}
	if !r.admit() {
		r.audit(ctx, model.EgressDeny, name, "dns: rate limit exceeded")
		return nil, fmt.Errorf("%w: %q", ErrRateLimited, name)
	}

	ips, err := r.cfg.Lookup(ctx, name)
	if err != nil {
		if errors.Is(err, ErrNXDOMAIN) && r.recordNXDOMAIN() {
			r.audit(ctx, model.EgressDeny, name, "dns: nxdomain_burst flagged")
		}
		r.audit(ctx, model.EgressDeny, name, "dns: lookup failed")
		return nil, fmt.Errorf("egress resolve %q: %w", name, err)
	}

	// Drop any unconditionally-denied IP (loopback, RFC1918/ULA, link-local,
	// the cloud-metadata endpoint, reserved ranges) BEFORE it is pinned or
	// returned. An allowlisted name that resolves to a denied IP is a DNS-rebind
	// attempt: the guest must never learn the denied IP (no answer leak) and it
	// must never be pinned, so the pin set matches exactly what the proxy will
	// admit (no over-wide pin honored on a later raw-IP connect). Refs: SEC-04, SEC-07
	allowed := filterDeniedIPs(ips)
	r.pin(name, allowed)
	r.audit(ctx, model.EgressAllow, name, "dns: resolved (pinned)")
	return allowed, nil
}

// filterDeniedIPs returns only the IPs that are not unconditionally denied,
// preserving order. The result may be empty (a name that resolved solely to
// denied ranges), which is a valid "no usable address" answer. Refs: SEC-04
func filterDeniedIPs(ips []netip.Addr) []netip.Addr {
	allowed := make([]netip.Addr, 0, len(ips))
	for _, ip := range ips {
		if _, denied := IsUnconditionallyDenied(ip); !denied {
			allowed = append(allowed, ip)
		}
	}
	return allowed
}

// Pinned returns the IPs a prior Resolve pinned for a name, if any. The
// egress proxy uses the pinned set so it connects to exactly what was
// resolved and audited (DNS-rebind defense). Refs: SEC-04
func (r *Resolver) Pinned(name string) ([]netip.Addr, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ips, ok := r.pins[name]
	return ips, ok
}

// IsPinned reports whether an IP was returned by a prior host-side
// resolution of an allowlisted name (and thus pinned). The egress proxy
// consults this so a client that resolved a name and then connects to the
// returned IP — or a transparent redirect that only sees the IP — is
// admitted, while an IP that was never resolved from an allowlisted name is
// not. Refs: SEC-04
func (r *Resolver) IsPinned(ip netip.Addr) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, ips := range r.pins {
		for _, p := range ips {
			if p == ip {
				return true
			}
		}
	}
	return false
}

// NXDOMAINBurst reports whether an NXDOMAIN burst has been flagged in the
// current window. Refs: SEC-07
func (r *Resolver) NXDOMAINBurst() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.nxFlagged
}

// admit applies the fixed-window rate cap, rolling the window on the
// injected clock. Caller must not hold the lock.
func (r *Resolver) admit() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.cfg.Clock()
	if r.windowStart.IsZero() || now.Sub(r.windowStart) >= r.window {
		r.windowStart = now
		r.queryCount = 0
		r.nxCount = 0
		r.nxFlagged = false
	}
	if r.queryCount >= r.maxQPW {
		return false
	}
	r.queryCount++
	return true
}

// recordNXDOMAIN counts one NXDOMAIN in the window and reports whether this
// is the moment the burst threshold is first crossed (so the flag is
// audited exactly once per window). Refs: SEC-07
func (r *Resolver) recordNXDOMAIN() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nxCount++
	if r.nxCount >= r.nxCap && !r.nxFlagged {
		r.nxFlagged = true
		return true
	}
	return false
}

// pin records the resolved IPs for a name.
func (r *Resolver) pin(name string, ips []netip.Addr) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pins[name] = ips
}

// audit appends one dns decision. The policy decision itself already stands by
// the time this is called (deny returns regardless), so a transient audit-write
// error does not change the outcome — but it must not be swallowed silently
// (CLAUDE.md "no swallowed errors"): a failed durable write is logged so the
// audit-trail gap is observable. The flow is not failed on the write error
// (the decision is already enforced). Refs: FR-17.8, FR-17.18
func (r *Resolver) audit(ctx context.Context, decision, name, rule string) {
	if err := r.cfg.Audit.AppendEgressRecord(ctx, &model.EgressRecord{
		SandboxID: r.cfg.SandboxID, TaskID: r.cfg.TaskID,
		Decision: decision, Protocol: "dns", DestHost: model.TruncateDestHost(name), Rule: rule,
	}); err != nil {
		r.logger.Error("egress dns audit write failed", "event", "egress_audit_writefail",
			"sandbox_id", r.cfg.SandboxID, "decision", decision, "error", err.Error())
	}
}

// orDefault returns v when positive, else def.
func orDefault(v, def int) int {
	if v > 0 {
		return v
	}
	return def
}
