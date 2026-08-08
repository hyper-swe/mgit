package egress

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// echoOnce serves one line and closes, standing in for a real destination.
func echoOnce(t *testing.T, banner string) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.WriteString(c, banner); _ = c.Close() }()
		}
	}()
	return ln
}

// transparentFixture builds a TransparentProxy whose redirected connections
// all claim the same original destination, with the real Authorizer behind it.
func transparentFixture(t *testing.T, allowlist []string, orig netip.AddrPort, target net.Listener) (*TransparentProxy, *recordingAuditor) {
	t.Helper()
	aud := &recordingAuditor{}
	al, err := Compile(allowlist)
	require.NoError(t, err)
	res, err := NewResolver(ResolverConfig{
		SandboxID: "sbx-t", TaskID: "MGIT-69", Allowlist: al,
		Lookup: resolvesTo(orig.Addr().String()), Audit: aud, Clock: frozenClock(),
	})
	require.NoError(t, err)
	az, err := NewAuthorizer(AuthorizerConfig{
		SandboxID: "sbx-t", TaskID: "MGIT-69",
		Allowlist: al, Resolver: res, Audit: aud,
	})
	require.NoError(t, err)

	p, err := NewTransparentProxy(TransparentProxyConfig{
		Authorizer: az,
		Dial: func(ctx context.Context, _ netip.Addr, _ int) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", target.Addr().String())
		},
		OriginalDst: func(net.Conn) (netip.AddrPort, error) { return orig, nil },
		Logger:      quietLogger(),
	})
	require.NoError(t, err)
	return p, aud
}

// serveTransparent runs the proxy on an ephemeral port and returns its addr.
func serveTransparent(t *testing.T, p *TransparentProxy) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); _ = ln.Close() })
	go func() { _ = p.Serve(ctx, ln) }()
	return ln.Addr().String()
}

// dialAndDrain connects and reads to EOF, returning whatever bytes arrived and
// the first error from EITHER the dial or the read.
//
// A refusal can surface at either point: SO_LINGER 0 makes the reset race the
// client's connect, so the same policy denial appears as a failed Dial on one
// platform and a failed Read on another. What must hold is the OUTCOME — no
// bytes, and an error that reads as a reset — not where it surfaced.
func dialAndDrain(t *testing.T, addr string) (string, error) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return "", err
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	got, err := io.ReadAll(conn)
	return string(got), err
}

// TestTransparentProxy_AllowlistedDestination_CarriesRealBytes is the MGIT-69
// allow assertion at the unit layer: an ordinary TCP connection — no proxy
// protocol, no client awareness — reaches an allowlisted destination and gets
// real bytes back.
//
// Before this, allowlist mode on firecracker permitted the guest to reach
// ONLY mgit's length-prefixed proxy port, and nothing in any guest speaks
// that protocol. So `npm install` and `curl` could not connect at all, while
// the e2e drove the proxy from the HOST and passed. Refs: MGIT-69, SEC-04
func TestTransparentProxy_AllowlistedDestination_CarriesRealBytes(t *testing.T) {
	target := echoOnce(t, "REAL-BYTES-FROM-DESTINATION")
	orig := netip.MustParseAddrPort("140.82.112.3:443")
	p, _ := transparentFixture(t, []string{"140.82.112.3:443"}, orig, target)
	addr := serveTransparent(t, p)

	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	got, err := io.ReadAll(conn)

	require.NoError(t, err)
	assert.Equal(t, "REAL-BYTES-FROM-DESTINATION", string(got),
		"an ordinary client must reach an allowlisted destination with no proxy awareness")
}

// TestTransparentProxy_OffAllowlist_IsRefused verifies the flow is REFUSED,
// and refused by policy: the connection is accepted by the kernel redirect
// and then closed by mgit, which is what a client sees as a reset rather than
// as an unreachable network. Refs: SEC-04, MGIT-69
func TestTransparentProxy_OffAllowlist_IsRefused(t *testing.T) {
	target := echoOnce(t, "SHOULD-NEVER-ARRIVE")
	orig := netip.MustParseAddrPort("140.82.112.3:443")
	// The policy names a DIFFERENT destination.
	p, aud := transparentFixture(t, []string{"1.2.3.4:443"}, orig, target)
	addr := serveTransparent(t, p)

	got, _ := dialAndDrain(t, addr)

	assert.Empty(t, got, "a denied flow must carry no bytes from the destination")
	// The audit is written by the proxy's own goroutine, which may still be
	// running when the client's reset arrives — so wait for it rather than
	// racing it.
	require.Eventually(t, func() bool {
		for _, r := range aud.snapshot() {
			if r.Decision == model.EgressDeny {
				return true
			}
		}
		return false
	}, 5*time.Second, 10*time.Millisecond, "a denied flow must be audited as a denial (FR-17.18)")
}

// TestTransparentProxy_PinnedNameResolution_IsHonored verifies the whole
// point of pairing the transparent redirect with the host resolver: the guest
// resolves an allowlisted NAME through mgit's DNS (which pins the address),
// then makes an ordinary connection to that address, and the redirect
// authorizes it because the IP was pinned — not because the IP itself is
// allowlisted. Refs: SEC-04, SEC-07, MGIT-69
func TestTransparentProxy_PinnedNameResolution_IsHonored(t *testing.T) {
	target := echoOnce(t, "PINNED-OK")
	orig := netip.MustParseAddrPort("140.82.112.3:443")
	aud := &recordingAuditor{}
	al, err := Compile([]string{"allowed.example:443"}) // a NAME, not an IP
	require.NoError(t, err)
	res, err := NewResolver(ResolverConfig{
		SandboxID: "sbx-t", TaskID: "MGIT-69", Allowlist: al,
		Lookup: resolvesTo("140.82.112.3"), Audit: aud, Clock: frozenClock(),
	})
	require.NoError(t, err)
	az, err := NewAuthorizer(AuthorizerConfig{
		SandboxID: "sbx-t", TaskID: "MGIT-69", Allowlist: al, Resolver: res, Audit: aud,
	})
	require.NoError(t, err)
	p, err := NewTransparentProxy(TransparentProxyConfig{
		Authorizer: az,
		Dial: func(ctx context.Context, _ netip.Addr, _ int) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", target.Addr().String())
		},
		OriginalDst: func(net.Conn) (netip.AddrPort, error) { return orig, nil },
		Logger:      quietLogger(),
	})
	require.NoError(t, err)
	addr := serveTransparent(t, p)

	// Without the resolution first, the raw IP is not allowlisted.
	before, _ := dialAndDrain(t, addr)
	assert.Empty(t, before, "a raw IP with no prior resolution is not allowlisted (SEC-04)")

	// The guest resolves the allowlisted name; the resolver pins the address.
	_, err = res.Resolve(context.Background(), "allowed.example")
	require.NoError(t, err)

	after, err := dialAndDrain(t, addr)

	require.NoError(t, err)
	assert.Equal(t, "PINNED-OK", after,
		"the pinned address of an allowlisted name must be reachable transparently")
}

// TestTransparentProxy_UnknownOriginalDst_IsRefused verifies a connection
// whose original destination cannot be recovered is dropped rather than
// forwarded somewhere guessed — the fail-closed rule for the one input this
// proxy cannot derive from policy.
func TestTransparentProxy_UnknownOriginalDst_IsRefused(t *testing.T) {
	target := echoOnce(t, "SHOULD-NEVER-ARRIVE")
	al, err := Compile([]string{"140.82.112.3:443"})
	require.NoError(t, err)
	aud := &recordingAuditor{}
	res, err := NewResolver(ResolverConfig{
		SandboxID: "sbx-t", TaskID: "MGIT-69", Allowlist: al,
		Lookup: resolvesTo("140.82.112.3"), Audit: aud, Clock: frozenClock(),
	})
	require.NoError(t, err)
	az, err := NewAuthorizer(AuthorizerConfig{
		SandboxID: "sbx-t", TaskID: "MGIT-69", Allowlist: al, Resolver: res, Audit: aud,
	})
	require.NoError(t, err)
	p, err := NewTransparentProxy(TransparentProxyConfig{
		Authorizer: az,
		Dial: func(ctx context.Context, _ netip.Addr, _ int) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", target.Addr().String())
		},
		OriginalDst: func(net.Conn) (netip.AddrPort, error) {
			return netip.AddrPort{}, errors.New("not a redirected connection")
		},
		Logger: quietLogger(),
	})
	require.NoError(t, err)
	addr := serveTransparent(t, p)

	got, _ := dialAndDrain(t, addr)

	assert.Empty(t, got)
}

// TestNewTransparentProxy_RequiresAnAuthorizer verifies it cannot be built
// without policy: a transparent proxy with no authorizer would forward
// everything the kernel redirected into it, which is an open network wearing
// an enforcement point's name.
func TestNewTransparentProxy_RequiresAnAuthorizer(t *testing.T) {
	_, err := NewTransparentProxy(TransparentProxyConfig{
		OriginalDst: func(net.Conn) (netip.AddrPort, error) { return netip.AddrPort{}, nil },
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "authorizer")
}

// TestTransparentProxy_DeniedFlow_LooksLikeARefusalNotAnEmptyReply verifies a
// denied flow is RESET, not closed cleanly.
//
// This is the MGIT-68 rule applied to the redirect path. The kernel completes
// the handshake before mgit sees the flow, so a plain close would be
// indistinguishable from a destination that answered with nothing — and from
// a dead network. A reset is what the guest would see from a genuine refusal.
// Refs: MGIT-69, SEC-04
func TestTransparentProxy_DeniedFlow_LooksLikeARefusalNotAnEmptyReply(t *testing.T) {
	target := echoOnce(t, "SHOULD-NEVER-ARRIVE")
	orig := netip.MustParseAddrPort("140.82.112.3:443")
	p, _ := transparentFixture(t, []string{"1.2.3.4:443"}, orig, target)
	addr := serveTransparent(t, p)

	got, err := dialAndDrain(t, addr)

	assert.Empty(t, got)
	require.Error(t, err, "a denied flow must not read as a clean, empty response")
	assert.Contains(t, err.Error(), "reset",
		"the client must see a reset, so a policy refusal is distinguishable from an empty reply")
}
