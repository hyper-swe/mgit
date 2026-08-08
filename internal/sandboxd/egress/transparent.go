package egress

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"time"
)

// transparentDialTimeout bounds the host-side connect to an authorized
// destination, so a black-holed destination cannot hold a guest connection —
// and a goroutine — open indefinitely.
const transparentDialTimeout = 10 * time.Second

// OriginalDstFunc recovers the destination a redirected connection was
// ORIGINALLY headed for, before the kernel rewrote it.
//
// It is a seam for two reasons: the mechanism is Linux-only
// (getsockopt SO_ORIGINAL_DST on the conntrack entry), and the whole
// authorize-and-splice path is worth testing without root, a tap device or
// iptables. Refs: MGIT-69, SEC-04
type OriginalDstFunc func(conn net.Conn) (netip.AddrPort, error)

// TransparentProxyConfig wires the transparent egress proxy.
type TransparentProxyConfig struct {
	Authorizer  *Authorizer
	Dial        DialFunc        // nil => HostDial
	OriginalDst OriginalDstFunc // nil => the platform's real mechanism
	Logger      *slog.Logger
	// Flows, when set, registers each spliced connection so a live policy
	// revoke can KILL it rather than only refusing the next one (MGIT-72).
	Flows *FlowRegistry
}

// TransparentProxy is the allowlist-mode egress path for guests that speak
// ordinary TCP — which is every guest.
//
// WHY IT EXISTS. Allowlist mode gave the guest no direct route and permitted
// it to reach exactly two host ports: mgit's DNS resolver, and mgit's
// length-prefixed CONNECT proxy. Nothing in any guest speaks that proxy
// protocol, no proxy environment variables were injected, and there was no
// redirect — so an ordinary program (npm, apt, curl, git) could not open a
// connection to ANY destination, allowlisted or not. The enforcement was
// real; there was simply no way through it. The e2e drove the proxy from the
// HOST, so it demonstrated the policy while never proving a guest could use
// it. That is the same shape as MGIT-68: a test that cannot tell "blocked"
// from "broken". Refs: MGIT-69, MGIT-68, SEC-04, FR-17.8
//
// HOW IT WORKS. The tap's nat PREROUTING chain REDIRECTs the guest's TCP to
// this listener on the gateway. The kernel rewrites the destination but
// conntrack remembers the original, which OriginalDst recovers. That original
// destination — host-observed, never guest-asserted (SEC-05) — is what the
// authorizer decides on, and an allowed flow is dialed to the PINNED address
// the authorizer returns, never a re-resolution (DNS-rebind defense).
//
// This makes firecracker's allowlist mode behave like libkrun's netstack
// gateway, which terminates guest connections in the same way and for the
// same reasons — one semantic for the same policy on both backends.
type TransparentProxy struct {
	auth    *Authorizer
	dial    DialFunc
	origDst OriginalDstFunc
	logger  *slog.Logger
	flows   *FlowRegistry
}

// NewTransparentProxy validates the configuration and returns the proxy. An
// authorizer is required: without one this would forward whatever the kernel
// redirected into it, which is an open network with an enforcement point's
// name on it.
func NewTransparentProxy(cfg TransparentProxyConfig) (*TransparentProxy, error) {
	if cfg.Authorizer == nil {
		return nil, fmt.Errorf("transparent proxy: authorizer must not be nil — " +
			"a transparent proxy with no policy is an open route, not an enforcement point")
	}
	p := &TransparentProxy{
		auth:    cfg.Authorizer,
		dial:    cfg.Dial,
		origDst: cfg.OriginalDst,
		logger:  cfg.Logger,
		flows:   cfg.Flows,
	}
	if p.dial == nil {
		p.dial = HostDial
	}
	if p.origDst == nil {
		p.origDst = OriginalDst
	}
	if p.logger == nil {
		p.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return p, nil
}

// Serve accepts redirected connections until ctx is canceled or the listener
// fails. Each connection is handled in its own goroutine so one slow
// destination cannot stall the rest of the guest's egress.
func (p *TransparentProxy) Serve(ctx context.Context, ln net.Listener) error {
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
				return fmt.Errorf("transparent proxy accept: %w", err)
			}
		}
		go p.handle(ctx, conn)
	}
}

// handle authorizes one redirected connection and splices it when allowed.
// Every refusal closes the guest's connection, which the guest sees as a
// reset — distinguishable from an unreachable network, which is what a guest
// with no route saw before. Refs: SEC-04, MGIT-69
func (p *TransparentProxy) handle(ctx context.Context, guest net.Conn) {
	dst, err := p.origDst(guest)
	if err != nil {
		// The one input this proxy cannot derive from policy. Fail closed:
		// forwarding to a guessed destination would be worse than refusing.
		p.logger.Warn("redirected connection has no recoverable original destination; refused",
			"event", "egress_transparent_no_origdst", "error", err.Error())
		refuse(guest)
		return
	}

	decision, err := p.auth.Authorize(ctx, Flow{
		Protocol: "tcp", Host: dst.Addr().String(), Port: int(dst.Port()),
	})
	if err != nil {
		// A policy DENY is an expected outcome the authorizer already audits;
		// an authorizer that could not decide is a fault. The guest sees the
		// same reset, but an operator needs to tell them apart.
		p.logger.Error("egress authorization failed; flow refused",
			"event", "egress_authorize_failed", "dest_ip", dst.Addr().String(),
			"dest_port", int(dst.Port()), "error", err.Error())
		refuse(guest)
		return
	}
	if !decision.Allow {
		refuse(guest)
		return
	}

	dialCtx, cancel := context.WithTimeout(ctx, transparentDialTimeout)
	defer cancel()
	// Connect to the PINNED destination the authorizer returned, never a
	// re-resolution (DNS-rebind defense). Refs: SEC-04
	outbound, err := p.dial(dialCtx, decision.DestIP, int(dst.Port()))
	if err != nil {
		p.logger.Warn("allowed egress flow could not be dialed; flow refused",
			"event", "egress_dial_failed", "dest_ip", decision.DestIP.String(),
			"dest_port", int(dst.Port()), "rule", decision.Rule, "error", err.Error())
		refuse(guest)
		return
	}
	// Tracked so a live revoke can terminate this flow (MGIT-72).
	SpliceTracked(p.flows, guest, outbound)
}

// refuse closes a redirected connection so the guest sees a RESET rather than
// a clean end-of-stream.
//
// The distinction is the MGIT-68 rule applied to this path: a policy refusal
// must be tellable apart from a dead network AND from a destination that
// simply had nothing to say. The kernel's REDIRECT already completed the
// handshake before mgit ever saw the flow, so a plain Close would look like a
// normal, empty response. SO_LINGER 0 makes the close a reset, which is what
// the guest would have seen had the connection been refused outright — and
// what libkrun's gateway produces for the same decision. Refs: MGIT-69, SEC-04
func refuse(conn net.Conn) {
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetLinger(0)
	}
	_ = conn.Close()
}
