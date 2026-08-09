package egress

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"time"

	"github.com/hyper-swe/mgit/internal/model"
)

// SupervisorConfig assembles a per-sandbox egress enforcement stack from the
// host policy. It is the seam the daemon calls when launching an
// allowlist-mode sandbox. Refs: SEC-04, FR-17.8
type SupervisorConfig struct {
	SandboxID string
	TaskID    string
	Policy    model.NetworkPolicy
	Audit     Auditor
	Lookup    LookupFunc // host-side resolver; SystemLookup in production
	Dial      DialFunc   // host-side dialer to authorized destinations
	Clock     func() time.Time
	Logger    *slog.Logger
	// OnDenial, when set, is forwarded to the authorizer to escalate denials
	// with a host-observed destination into capability requests (FR-17.12).
	OnDenial func(model.ObservedDenial)
}

// Supervisor owns one sandbox's egress stack: the compiled allowlist, the
// host-side restricted resolver, the flow authorizer, and the CONNECT
// proxy. The daemon serves Proxy() on the per-sandbox egress channel and
// drives Resolver() for the guest's DNS. Refs: SEC-04, FR-17.8
type Supervisor struct {
	allowlist  *Allowlist
	resolver   *Resolver
	authorizer *Authorizer
	proxy      *Proxy
	dns        *DNSServer
	// flows tracks this sandbox's live spliced connections so a live policy
	// revoke can terminate established flows, not merely refuse the next one.
	// Refs: MGIT-72
	flows *FlowRegistry
}

// NewSupervisor builds the allowlist-mode egress stack. It is an error to
// build one for any other mode: none attaches no NIC and open uses host
// NAT, so neither runs a proxy — the caller selects per mode. A malformed
// allowlist fails the build (fail closed, before the sandbox runs).
// Refs: SEC-04, FR-17.7, FR-17.8
func NewSupervisor(cfg SupervisorConfig) (*Supervisor, error) {
	if cfg.Policy.Mode != model.NetworkModeAllowlist {
		return nil, fmt.Errorf("egress supervisor: only allowlist mode runs a proxy, got %q", cfg.Policy.Mode)
	}
	switch {
	case cfg.Audit == nil:
		return nil, fmt.Errorf("egress supervisor: auditor must not be nil")
	case cfg.Lookup == nil:
		return nil, fmt.Errorf("egress supervisor: lookup must not be nil")
	case cfg.Dial == nil:
		return nil, fmt.Errorf("egress supervisor: dialer must not be nil")
	case cfg.Clock == nil:
		return nil, fmt.Errorf("egress supervisor: clock must not be nil")
	case cfg.SandboxID == "":
		return nil, fmt.Errorf("egress supervisor: sandbox id must not be empty")
	case cfg.Logger == nil:
		return nil, fmt.Errorf("egress supervisor: logger must not be nil")
	}

	al, err := Compile(cfg.Policy.Allowlist)
	if err != nil {
		return nil, fmt.Errorf("egress supervisor: %w", err)
	}
	resolver, err := NewResolver(ResolverConfig{
		SandboxID: cfg.SandboxID, TaskID: cfg.TaskID,
		Allowlist: al, Lookup: cfg.Lookup, Audit: cfg.Audit, Clock: cfg.Clock,
		Logger: cfg.Logger,
	})
	if err != nil {
		return nil, fmt.Errorf("egress supervisor: %w", err)
	}
	authorizer, err := NewAuthorizer(AuthorizerConfig{
		SandboxID: cfg.SandboxID, TaskID: cfg.TaskID,
		Allowlist: al, Resolver: resolver, Audit: cfg.Audit,
		Logger: cfg.Logger, OnDenial: cfg.OnDenial,
	})
	if err != nil {
		return nil, fmt.Errorf("egress supervisor: %w", err)
	}
	flows := NewFlowRegistry()
	proxy, err := NewProxy(ProxyConfig{
		Authorizer: authorizer, Dial: cfg.Dial, Logger: cfg.Logger, Flows: flows})
	if err != nil {
		return nil, fmt.Errorf("egress supervisor: %w", err)
	}
	dns, err := NewDNSServer(resolver, cfg.Logger)
	if err != nil {
		return nil, fmt.Errorf("egress supervisor: %w", err)
	}
	return &Supervisor{allowlist: al, resolver: resolver, authorizer: authorizer,
		proxy: proxy, dns: dns, flows: flows}, nil
}

// Authorizer returns the assembled flow authorizer, for enforcement points
// that terminate the guest's connections themselves instead of serving the
// CONNECT proxy — the libkrun netstack gateway consults it per flow.
// Refs: SEC-04, ADR-010
func (s *Supervisor) Authorizer() *Authorizer { return s.authorizer }

// Allowlist returns this sandbox's compiled allowlist so the daemon can apply
// a host-approved, sandbox-lifetime capability grant (Allowlist.GrantIP) to
// the LIVE enforcement path. Refs: FR-17.12, SEC-05
func (s *Supervisor) Allowlist() *Allowlist { return s.allowlist }

// Flows returns this sandbox's live-flow registry, so a policy revoke can kill
// established connections (MGIT-72) and an enforcement point that splices
// flows itself can register them.
func (s *Supervisor) Flows() *FlowRegistry { return s.flows }

// Proxy returns the assembled egress proxy (served on the sandbox's egress
// channel). Refs: SEC-04
func (s *Supervisor) Proxy() *Proxy { return s.proxy }

// Resolver returns the host-side restricted resolver (driven for the
// guest's DNS). Refs: SEC-07
func (s *Supervisor) Resolver() *Resolver { return s.resolver }

// DNS returns the restricted DNS server (served on the sandbox gateway's
// :53 so the guest resolves only allowlisted names). Refs: SEC-07
func (s *Supervisor) DNS() *DNSServer { return s.dns }

// HostDial is the production DialFunc: it opens the authorized host-side
// connection to a destination the authorizer approved, to the PINNED IP it
// returned (never a re-resolution). It is exported for the same reason
// SystemLookup is — every enforcement point must dial the same way, whether
// it runs in the daemon (the CONNECT proxy) or in a VM's own process (the
// libkrun netstack gateway). A timeout or address-family rule added here
// then reaches both. Refs: SEC-04
func HostDial(ctx context.Context, ip netip.Addr, port int) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "tcp", netip.AddrPortFrom(ip, uint16(port)).String()) //nolint:gosec // OK: the authorizer range-checks the port
}

// SystemLookup adapts a *net.Resolver to LookupFunc, resolving on the HOST
// and mapping a not-found result to ErrNXDOMAIN so the resolver can count
// label-enumeration bursts. A nil resolver uses the default. Refs: SEC-07
func SystemLookup(resolver *net.Resolver) LookupFunc {
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	return func(ctx context.Context, name string) ([]netip.Addr, error) {
		addrs, err := resolver.LookupNetIP(ctx, "ip", name)
		if err != nil {
			return nil, mapLookupError(err)
		}
		out := make([]netip.Addr, 0, len(addrs))
		for _, a := range addrs {
			out = append(out, a.Unmap())
		}
		return out, nil
	}
}

// mapLookupError maps a not-found DNS error to ErrNXDOMAIN, leaving other
// failures (timeout, server error) as-is. Refs: SEC-07
func mapLookupError(err error) error {
	if err == nil {
		return nil
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
		return fmt.Errorf("%w: %s", ErrNXDOMAIN, dnsErr.Err)
	}
	return err
}
