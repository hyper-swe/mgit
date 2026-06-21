package egress

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestConstructors_FailClosedOnMissingDeps verifies every egress component
// refuses to build without its required dependencies — a misconfigured
// enforcement path must fail closed, never silently disable a control.
// Refs: SEC-04, FR-17.8
func TestConstructors_FailClosedOnMissingDeps(t *testing.T) {
	al, err := Compile([]string{"registry.npmjs.org"})
	require.NoError(t, err)
	aud := &fakeAuditor{}
	lookup := resolvesTo("140.82.112.3")
	clock := frozenClock()
	goodRes, err := NewResolver(ResolverConfig{SandboxID: "01SB", Allowlist: al, Lookup: lookup, Audit: aud, Clock: clock})
	require.NoError(t, err)

	t.Run("resolver", func(t *testing.T) {
		base := ResolverConfig{SandboxID: "01SB", Allowlist: al, Lookup: lookup, Audit: aud, Clock: clock}
		for name, mutate := range map[string]func(*ResolverConfig){
			"nil_allowlist": func(c *ResolverConfig) { c.Allowlist = nil },
			"nil_lookup":    func(c *ResolverConfig) { c.Lookup = nil },
			"nil_audit":     func(c *ResolverConfig) { c.Audit = nil },
			"nil_clock":     func(c *ResolverConfig) { c.Clock = nil },
			"empty_id":      func(c *ResolverConfig) { c.SandboxID = "" },
		} {
			t.Run(name, func(t *testing.T) {
				cfg := base
				mutate(&cfg)
				_, err := NewResolver(cfg)
				assert.Error(t, err)
			})
		}
	})

	t.Run("authorizer", func(t *testing.T) {
		base := AuthorizerConfig{SandboxID: "01SB", Allowlist: al, Resolver: goodRes, Audit: aud}
		for name, mutate := range map[string]func(*AuthorizerConfig){
			"nil_allowlist": func(c *AuthorizerConfig) { c.Allowlist = nil },
			"nil_resolver":  func(c *AuthorizerConfig) { c.Resolver = nil },
			"nil_audit":     func(c *AuthorizerConfig) { c.Audit = nil },
			"empty_id":      func(c *AuthorizerConfig) { c.SandboxID = "" },
		} {
			t.Run(name, func(t *testing.T) {
				cfg := base
				mutate(&cfg)
				_, err := NewAuthorizer(cfg)
				assert.Error(t, err)
			})
		}
	})

	t.Run("proxy", func(t *testing.T) {
		az, err := NewAuthorizer(AuthorizerConfig{SandboxID: "01SB", Allowlist: al, Resolver: goodRes, Audit: aud})
		require.NoError(t, err)
		good := ProxyConfig{Authorizer: az, Dial: (&dialRecorder{}).dial, Logger: quietLogger()}
		for name, mutate := range map[string]func(*ProxyConfig){
			"nil_authorizer": func(c *ProxyConfig) { c.Authorizer = nil },
			"nil_dial":       func(c *ProxyConfig) { c.Dial = nil },
			"nil_logger":     func(c *ProxyConfig) { c.Logger = nil },
		} {
			t.Run(name, func(t *testing.T) {
				cfg := good
				mutate(&cfg)
				_, err := NewProxy(cfg)
				assert.Error(t, err)
			})
		}
		// the unmutated config builds, and defaults the handshake timeout.
		p, err := NewProxy(good)
		require.NoError(t, err)
		assert.Equal(t, 30*time.Second, p.cfg.HandshakeTimeout)
	})
}

// TestResolver_PinsResolvedIPs verifies a successful resolve pins the IPs so
// the proxy connects to exactly what was resolved (DNS-rebind defense).
// Refs: SEC-04, SEC-07
func TestResolver_PinsResolvedIPs(t *testing.T) {
	r := testResolver(t, resolvesTo("140.82.112.3"), &fakeAuditor{}, frozenClock())
	_, ok := r.Pinned("registry.npmjs.org")
	assert.False(t, ok, "nothing pinned before resolution")

	_, err := r.Resolve(context.Background(), "registry.npmjs.org")
	require.NoError(t, err)
	ips, ok := r.Pinned("registry.npmjs.org")
	require.True(t, ok, "a resolved name is pinned")
	require.Len(t, ips, 1)
	assert.Equal(t, "140.82.112.3", ips[0].String())
}

// TestAllowlist_PortNotAllowlistedForName_Denied verifies an allowlisted
// name reached on a non-allowlisted port is denied even though it resolves.
// Refs: SEC-04
func TestAllowlist_PortNotAllowlistedForName_Denied(t *testing.T) {
	aud := &fakeAuditor{}
	az := buildAuthorizer(t, []string{"pypi.org:443"}, resolvesTo("151.101.0.223"), aud)
	d, err := az.Authorize(context.Background(), Flow{Protocol: "tcp", Host: "pypi.org", Port: 22})
	assert.ErrorIs(t, err, ErrEgressDenied, "pypi.org:443 forbids port 22 even after resolution")
	assert.False(t, d.Allow)
}

// TestAuthorize_InvalidPort_Denied covers the port-range guard.
func TestAuthorize_InvalidPort_Denied(t *testing.T) {
	az := buildAuthorizer(t, []string{"registry.npmjs.org"}, resolvesTo("140.82.112.3"), &fakeAuditor{})
	_, err := az.Authorize(context.Background(), Flow{Protocol: "tcp", Host: "registry.npmjs.org", Port: 0})
	assert.ErrorIs(t, err, ErrEgressDenied)
}
