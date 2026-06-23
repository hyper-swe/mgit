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
}

// Supervisor owns one sandbox's egress stack: the compiled allowlist, the
// host-side restricted resolver, the flow authorizer, and the CONNECT
// proxy. The daemon serves Proxy() on the per-sandbox egress channel and
// drives Resolver() for the guest's DNS. Refs: SEC-04, FR-17.8
type Supervisor struct {
	allowlist *Allowlist
	resolver  *Resolver
	proxy     *Proxy
	dns       *DNSServer
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
	})
	if err != nil {
		return nil, fmt.Errorf("egress supervisor: %w", err)
	}
	authorizer, err := NewAuthorizer(AuthorizerConfig{
		SandboxID: cfg.SandboxID, TaskID: cfg.TaskID,
		Allowlist: al, Resolver: resolver, Audit: cfg.Audit,
	})
	if err != nil {
		return nil, fmt.Errorf("egress supervisor: %w", err)
	}
	proxy, err := NewProxy(ProxyConfig{Authorizer: authorizer, Dial: cfg.Dial, Logger: cfg.Logger})
	if err != nil {
		return nil, fmt.Errorf("egress supervisor: %w", err)
	}
	dns, err := NewDNSServer(resolver, cfg.Logger)
	if err != nil {
		return nil, fmt.Errorf("egress supervisor: %w", err)
	}
	return &Supervisor{allowlist: al, resolver: resolver, proxy: proxy, dns: dns}, nil
}

// Allowlist returns this sandbox's compiled allowlist so the daemon can apply
// a host-approved, sandbox-lifetime capability grant (Allowlist.GrantIP) to
// the LIVE enforcement path. Refs: FR-17.12, SEC-05
func (s *Supervisor) Allowlist() *Allowlist { return s.allowlist }

// Proxy returns the assembled egress proxy (served on the sandbox's egress
// channel). Refs: SEC-04
func (s *Supervisor) Proxy() *Proxy { return s.proxy }

// Resolver returns the host-side restricted resolver (driven for the
// guest's DNS). Refs: SEC-07
func (s *Supervisor) Resolver() *Resolver { return s.resolver }

// DNS returns the restricted DNS server (served on the sandbox gateway's
// :53 so the guest resolves only allowlisted names). Refs: SEC-07
func (s *Supervisor) DNS() *DNSServer { return s.dns }

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
