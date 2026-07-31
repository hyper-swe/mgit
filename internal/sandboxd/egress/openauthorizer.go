package egress

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/netip"

	"github.com/hyper-swe/mgit/internal/model"
)

// OpenAuthorizerConfig wires the open-mode authorizer.
type OpenAuthorizerConfig struct {
	SandboxID string
	TaskID    string
	Audit     Auditor
	// Logger records audit-write failures so a dropped durable record is
	// never silent. Optional; nil discards.
	Logger *slog.Logger
}

// OpenAuthorizer is the egress policy for `open` mode: every destination is
// permitted, and every flow is still RECORDED.
//
// Why this exists at all, when "open" means no restriction: the netstack
// gateway refuses to run without an authorizer, and rightly — a gateway
// holding no policy object is indistinguishable from a bug that dropped the
// policy, and that failure mode is an unaudited open network. Open mode needs
// an object that says "allow, and write it down".
//
// The audit is a genuine GAIN over what open mode used to be. The iptables
// NAT path this replaces could not produce per-flow records at all: the
// kernel forwarded packets and nothing in mgit saw a connection. Here the
// gateway terminates every connection, so open mode gets the same per-flow
// trail as allowlist mode — same shape, different verdict.
//
// "Any destination" is NOT "any protocol": flows the data path cannot carry
// are still refused, because admitting them would promise something the
// gateway does not deliver. Refs: SEC-04, FR-17.8, MGIT-61.9
type OpenAuthorizer struct {
	cfg    OpenAuthorizerConfig
	logger *slog.Logger
}

// NewOpenAuthorizer validates the configuration and returns an OpenAuthorizer.
// An auditor and a sandbox ID are both required: a record nobody can attribute
// to a sandbox is not an audit trail.
func NewOpenAuthorizer(cfg OpenAuthorizerConfig) (*OpenAuthorizer, error) {
	switch {
	case cfg.Audit == nil:
		return nil, fmt.Errorf("egress open authorizer: auditor must not be nil")
	case cfg.SandboxID == "":
		return nil, fmt.Errorf("egress open authorizer: sandbox id must not be empty")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &OpenAuthorizer{cfg: cfg, logger: logger}, nil
}

// Authorize admits any destination the gateway can actually reach, and audits
// the decision either way.
//
// The refusals mirror the allowlist authorizer's, and for the same reason
// rather than for symmetry: the gateway registers TCP only and binds exactly
// one UDP endpoint (the pinned DNS resolver), so a UDP flow has no listener
// and would be dropped after being "allowed". A host that is not a literal IP
// cannot be reached either, because open mode does no host-side resolution —
// the guest resolves through the gateway's DNS and then connects by address.
// Refs: SEC-04, FR-17.8
func (a *OpenAuthorizer) Authorize(ctx context.Context, f Flow) (Decision, error) {
	if f.Port < 1 || f.Port > 65535 {
		return a.deny(ctx, f, netip.Addr{}, "invalid port")
	}
	if f.Protocol != "tcp" {
		return a.deny(ctx, f, netip.Addr{}, "non-tcp blocked (quic/udp have no egress path)")
	}
	ip, err := netip.ParseAddr(f.Host)
	if err != nil {
		return a.deny(ctx, f, netip.Addr{}, "open mode connects by address; no host-side resolution")
	}
	return a.allow(ctx, f, ip.Unmap())
}

// allow records and returns an allow decision, pinning the requested address
// as the destination the gateway must dial.
func (a *OpenAuthorizer) allow(ctx context.Context, f Flow, ip netip.Addr) (Decision, error) {
	a.audit(ctx, model.EgressAllow, f, ip, "open mode: unrestricted egress")
	return Decision{Allow: true, DestIP: ip, Rule: "open mode: unrestricted egress"}, nil
}

// deny records and returns a deny decision wrapping ErrEgressDenied.
func (a *OpenAuthorizer) deny(ctx context.Context, f Flow, ip netip.Addr, rule string) (Decision, error) {
	a.audit(ctx, model.EgressDeny, f, ip, rule)
	return Decision{Allow: false, Rule: rule},
		fmt.Errorf("%w: %s:%d (%s)", ErrEgressDenied, f.Host, f.Port, rule)
}

// audit appends one egress decision. A failed durable write does not change
// the outcome — the decision already stands — but it is logged rather than
// swallowed, so a gap in the trail is observable. Refs: FR-17.8, FR-17.18
func (a *OpenAuthorizer) audit(ctx context.Context, decision string, f Flow, ip netip.Addr, rule string) {
	rec := &model.EgressRecord{
		SandboxID: a.cfg.SandboxID, TaskID: a.cfg.TaskID,
		Decision: decision, Protocol: "tcp", DestHost: model.TruncateDestHost(f.Host),
		DestPort: f.Port, Rule: rule,
	}
	if ip.IsValid() {
		rec.DestIP = ip.String()
	}
	if err := a.cfg.Audit.AppendEgressRecord(ctx, rec); err != nil {
		a.logger.Error("egress open-mode audit write failed", "event", "egress_audit_writefail",
			"sandbox_id", a.cfg.SandboxID, "decision", decision, "error", err.Error())
	}
}
