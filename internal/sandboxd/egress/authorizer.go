package egress

import (
	"context"
	"fmt"
	"net/netip"

	"github.com/hyper-swe/mgit/internal/model"
)

// Flow is one egress attempt as the host observes it: the requested host
// (a name or an IP literal) and port, and the L4 protocol. Host and port
// are guest-supplied; the authorizer never trusts them beyond using the
// name to drive HOST-side resolution (SEC-04). Refs: SEC-04, FR-17.8
type Flow struct {
	Protocol string // "tcp" (permitted) or "udp" (blocked: QUIC/UDP have no egress path)
	Host     string // requested name or IP literal
	Port     int
}

// Decision is the authorizer's verdict. On allow, DestIP is the
// host-resolved (or literal) destination the proxy must connect to — the
// pinned IP, never a re-resolution. Rule records the matched entry or the
// deny reason for the audit trail. Refs: SEC-04, FR-17.8
type Decision struct {
	Allow  bool
	DestIP netip.Addr
	Rule   string
}

// AuthorizerConfig wires the flow authorizer.
type AuthorizerConfig struct {
	SandboxID string
	TaskID    string
	Allowlist *Allowlist
	Resolver  *Resolver
	Audit     Auditor
	// OnDenial, when set, is notified of each denial that names a concrete
	// host-observed destination IP, so it can be escalated to a capability
	// request (the deny->prompt trigger, FR-17.12). It carries only
	// host-observed facts (SEC-05) and must not block the deny path. Optional.
	OnDenial func(model.ObservedDenial)
}

// Authorizer decides each guest egress flow against the host allowlist,
// the unconditional IP denials (SEC-04/T9), and the host-side pinned
// resolver, and audits every allow and deny. It is the policy core the
// proxy consults before opening any host-side connection. Refs: SEC-04, FR-17.8
type Authorizer struct {
	cfg AuthorizerConfig
}

// NewAuthorizer validates the configuration and returns an Authorizer.
func NewAuthorizer(cfg AuthorizerConfig) (*Authorizer, error) {
	switch {
	case cfg.Allowlist == nil:
		return nil, fmt.Errorf("egress authorizer: allowlist must not be nil")
	case cfg.Resolver == nil:
		return nil, fmt.Errorf("egress authorizer: resolver must not be nil")
	case cfg.Audit == nil:
		return nil, fmt.Errorf("egress authorizer: auditor must not be nil")
	case cfg.SandboxID == "":
		return nil, fmt.Errorf("egress authorizer: sandbox id must not be empty")
	}
	return &Authorizer{cfg: cfg}, nil
}

// Authorize decides one flow. TCP only: UDP/QUIC is refused (the sole UDP
// egress is DNS to the host resolver, handled separately). A literal IP is
// a raw-IP attempt — admitted only by an IP/CIDR allowlist entry and never
// when in a denied range. A name is resolved host-side (allowlist-gated,
// pinned); the connection targets the pinned IP, and any pinned IP in a
// denied range is refused (DNS-rebind defense). Refs: SEC-04, FR-17.8
func (a *Authorizer) Authorize(ctx context.Context, f Flow) (Decision, error) {
	if f.Port < 1 || f.Port > 65535 {
		return a.deny(ctx, f, netip.Addr{}, "invalid port")
	}
	if f.Protocol != "tcp" {
		return a.deny(ctx, f, netip.Addr{}, "non-tcp blocked (quic/udp have no egress path)")
	}
	if ip, err := netip.ParseAddr(f.Host); err == nil {
		return a.authorizeRawIP(ctx, f, ip.Unmap())
	}
	return a.authorizeName(ctx, f)
}

// authorizeRawIP admits a literal-IP connection after the unconditional
// denials, either via an explicit IP/CIDR allowlist entry or because the IP
// was PINNED by a prior host-side resolution of an allowlisted name. The
// latter is what makes a normal "resolve-then-connect-by-IP" client (and a
// transparent proxy that only sees the destination IP) work: the IP is only
// pinned if the host itself resolved it from an allowlisted name, so a
// hardcoded C2 IP that was never resolved is still denied (SEC-04 raw-IP
// bypass). Refs: SEC-04
func (a *Authorizer) authorizeRawIP(ctx context.Context, f Flow, ip netip.Addr) (Decision, error) {
	if reason, denied := IsUnconditionallyDenied(ip); denied {
		return a.deny(ctx, f, ip, "denied range: "+reason)
	}
	if a.cfg.Allowlist.AllowsIP(ip, f.Port) {
		return a.allow(ctx, f, ip, "ip allowlisted")
	}
	if a.cfg.Resolver.IsPinned(ip) {
		return a.allow(ctx, f, ip, "pinned by a prior allowlisted resolution")
	}
	return a.deny(ctx, f, ip, "raw-ip not allowlisted (host-side DNS bypassed)")
}

// authorizeName resolves an allowlisted name host-side and targets a
// surviving pinned IP. The allowlist (name AND port) is checked BEFORE
// resolving, so a flow to an allowlisted name on a forbidden port is denied
// without consuming the DNS rate-limit budget or emitting a resolution
// (SEC-07). Refs: SEC-04, SEC-07
func (a *Authorizer) authorizeName(ctx context.Context, f Flow) (Decision, error) {
	if !a.cfg.Allowlist.AllowsName(f.Host, f.Port) {
		return a.deny(ctx, f, netip.Addr{}, "name/port not allowlisted")
	}
	ips, err := a.cfg.Resolver.Resolve(ctx, f.Host)
	if err != nil {
		return a.deny(ctx, f, netip.Addr{}, "name resolution refused")
	}
	for _, ip := range ips {
		ip = ip.Unmap()
		if _, denied := IsUnconditionallyDenied(ip); !denied {
			return a.allow(ctx, f, ip, "name allowlisted (pinned)")
		}
	}
	return a.deny(ctx, f, netip.Addr{}, "name resolved only to denied ranges (rebind)")
}

// allow records and returns an allow decision.
func (a *Authorizer) allow(ctx context.Context, f Flow, ip netip.Addr, rule string) (Decision, error) {
	a.audit(ctx, model.EgressAllow, f, ip, rule)
	return Decision{Allow: true, DestIP: ip, Rule: rule}, nil
}

// deny records and returns a deny decision wrapping ErrEgressDenied. When a
// concrete host-observed destination IP was resolved/parsed, it also notifies
// the optional escalation observer (the deny->prompt trigger) with host-only
// facts (SEC-05); a name denied before host resolution has no IP and is not
// escalatable. Refs: SEC-04, SEC-05, FR-17.12
func (a *Authorizer) deny(ctx context.Context, f Flow, ip netip.Addr, rule string) (Decision, error) {
	a.audit(ctx, model.EgressDeny, f, ip, rule)
	if a.cfg.OnDenial != nil && ip.IsValid() {
		a.cfg.OnDenial(model.ObservedDenial{
			SandboxID: a.cfg.SandboxID, TaskID: a.cfg.TaskID,
			DestIP: ip, DestPort: f.Port, Rule: rule,
		})
	}
	return Decision{Allow: false, Rule: rule}, fmt.Errorf("%w: %s:%d (%s)", ErrEgressDenied, f.Host, f.Port, rule)
}

// audit appends one tcp egress decision. DestIP is recorded only when a
// concrete host-observed IP was resolved/parsed. Refs: FR-17.8
func (a *Authorizer) audit(ctx context.Context, decision string, f Flow, ip netip.Addr, rule string) {
	rec := &model.EgressRecord{
		SandboxID: a.cfg.SandboxID, TaskID: a.cfg.TaskID,
		Decision: decision, Protocol: "tcp", DestHost: f.Host, DestPort: f.Port, Rule: rule,
	}
	if ip.IsValid() {
		rec.DestIP = ip.String()
	}
	_ = a.cfg.Audit.AppendEgressRecord(ctx, rec)
}
