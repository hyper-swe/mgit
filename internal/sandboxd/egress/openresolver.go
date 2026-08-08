package egress

import (
	"fmt"
	"log/slog"
	"time"
)

// OpenDNSConfig wires the open-mode DNS server. It is the resolver half of
// what OpenAuthorizerConfig is for flows: same machinery, different verdict.
type OpenDNSConfig struct {
	SandboxID string
	TaskID    string
	Audit     Auditor
	Lookup    LookupFunc // nil => the host's own resolver (SystemLookup)
	Clock     func() time.Time
	Logger    *slog.Logger
}

// NewOpenDNSServer builds the DNS server an OPEN-mode sandbox serves on its
// gateway: any name resolves, and every resolution is still recorded.
//
// WHY OPEN MODE HAS A RESOLVER AT ALL. Open mode used to bind none — the
// reasoning being that an unrestricted guest can resolve for itself. But the
// guest is TOLD its nameserver is the gateway (that is the one rule that
// makes a single resolv.conf descriptor correct on every backend), so with
// nothing listening there, every name in open mode failed with "connection
// refused" against a dead port. The tests could not see it because the
// open-mode assertions dialed a RAW IP. Refs: MGIT-69, FR-17.7
//
// Serving it here is also a security GAIN, not merely a fix: open-mode name
// resolution becomes auditable, rate-limited (SEC-07 anti-tunnel) and subject
// to the unconditional IP denials, none of which is true of a guest resolving
// through NAT to somewhere of its own choosing. Open mode means "any public
// destination", never "the host's LAN or metadata endpoint".
//
// It is one constructor so the two backends cannot assemble open-mode DNS two
// different ways — the drift that put them in disagreement before.
// Refs: MGIT-69, SEC-04, SEC-07, FR-17.18
func NewOpenDNSServer(cfg OpenDNSConfig) (*DNSServer, error) {
	lookup := cfg.Lookup
	if lookup == nil {
		lookup = SystemLookup(nil)
	}
	resolver, err := NewResolver(ResolverConfig{
		SandboxID:  cfg.SandboxID,
		TaskID:     cfg.TaskID,
		ResolveAny: true, // open mode: the gate is lifted, the controls are not
		Lookup:     lookup,
		Audit:      cfg.Audit,
		Clock:      cfg.Clock,
		Logger:     cfg.Logger,
	})
	if err != nil {
		return nil, fmt.Errorf("open-mode dns: %w", err)
	}
	return NewDNSServer(resolver, cfg.Logger)
}
