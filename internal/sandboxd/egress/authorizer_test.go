package egress

import (
	"context"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// buildAuthorizer wires an authorizer over a compiled allowlist and a
// resolver whose upstream lookup is the supplied fake.
func buildAuthorizer(t *testing.T, entries []string, lookup LookupFunc, aud Auditor) *Authorizer {
	t.Helper()
	al, err := Compile(entries)
	require.NoError(t, err)
	if lookup == nil {
		lookup = func(context.Context, string) ([]netip.Addr, error) { return nil, ErrNXDOMAIN }
	}
	res, err := NewResolver(ResolverConfig{
		SandboxID: "01SB", TaskID: "MGIT-11.7.2",
		Allowlist: al, Lookup: lookup, Audit: aud, Clock: frozenClock(),
	})
	require.NoError(t, err)
	az, err := NewAuthorizer(AuthorizerConfig{
		SandboxID: "01SB", TaskID: "MGIT-11.7.2",
		Allowlist: al, Resolver: res, Audit: aud,
	})
	require.NoError(t, err)
	return az
}

func resolvesTo(ip string) LookupFunc {
	return func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr(ip)}, nil
	}
}

// TestAllowlist_NonListedIP_Denied verifies a connection to a public IP
// that is not on the allowlist is dropped. Refs: SEC-04, MGIT-11.7.2
func TestAllowlist_NonListedIP_Denied(t *testing.T) {
	aud := &fakeAuditor{}
	az := buildAuthorizer(t, []string{"registry.npmjs.org"}, resolvesTo("140.82.112.3"), aud)

	d, err := az.Authorize(context.Background(), Flow{Protocol: "tcp", Host: "1.2.3.4", Port: 443})
	assert.ErrorIs(t, err, ErrEgressDenied)
	assert.False(t, d.Allow, "a non-allowlisted public IP is dropped")
}

// TestAllowlist_RawIPBypass_Denied verifies a guest cannot bypass host-side
// DNS by connecting to an allowlisted host's IP directly: only the NAME is
// allowlisted, so the raw-IP connection is refused (SEC-04 raw-IP bypass).
// Refs: SEC-04, MGIT-11.7.2
func TestAllowlist_RawIPBypass_Denied(t *testing.T) {
	aud := &fakeAuditor{}
	// registry.npmjs.org resolves to 140.82.112.3, but only the NAME is listed.
	az := buildAuthorizer(t, []string{"registry.npmjs.org"}, resolvesTo("140.82.112.3"), aud)

	d, err := az.Authorize(context.Background(), Flow{Protocol: "tcp", Host: "140.82.112.3", Port: 443})
	assert.ErrorIs(t, err, ErrEgressDenied, "a raw-IP connection to an allowlisted-by-name host is denied")
	assert.False(t, d.Allow)
}

// TestAllowlist_AllowsListedNameAndIP verifies the positive paths: an
// allowlisted name resolves and connects, and an explicitly allowlisted IP
// connects raw. Refs: SEC-04, FR-17.8
func TestAllowlist_AllowsListedNameAndIP(t *testing.T) {
	aud := &fakeAuditor{}
	az := buildAuthorizer(t, []string{"registry.npmjs.org", "8.8.8.8"}, resolvesTo("140.82.112.3"), aud)

	d, err := az.Authorize(context.Background(), Flow{Protocol: "tcp", Host: "registry.npmjs.org", Port: 443})
	require.NoError(t, err)
	assert.True(t, d.Allow)
	assert.Equal(t, "140.82.112.3", d.DestIP.String(), "the proxy connects to the host-resolved (pinned) IP")

	d, err = az.Authorize(context.Background(), Flow{Protocol: "tcp", Host: "8.8.8.8", Port: 443})
	require.NoError(t, err)
	assert.True(t, d.Allow, "an explicitly allowlisted IP connects raw")
}

// TestAllowlist_MetadataIP_AlwaysDenied verifies the cloud metadata
// endpoint is denied even when an allowlist entry names it (raw IP) and
// even when an allowlisted name resolves to it (DNS-rebind). Refs: SEC-04, T9
func TestAllowlist_MetadataIP_AlwaysDenied(t *testing.T) {
	// (a) metadata IP explicitly in the allowlist — unconditional deny wins.
	aud := &fakeAuditor{}
	az := buildAuthorizer(t, []string{"169.254.169.254"}, nil, aud)
	d, err := az.Authorize(context.Background(), Flow{Protocol: "tcp", Host: "169.254.169.254", Port: 80})
	assert.ErrorIs(t, err, ErrEgressDenied)
	assert.False(t, d.Allow, "metadata IP denied even when allowlisted by IP")

	// (b) an allowlisted name that resolves to the metadata IP (rebind).
	aud2 := &fakeAuditor{}
	az2 := buildAuthorizer(t, []string{"metadata.evil.example"}, resolvesTo("169.254.169.254"), aud2)
	d, err = az2.Authorize(context.Background(), Flow{Protocol: "tcp", Host: "metadata.evil.example", Port: 80})
	assert.ErrorIs(t, err, ErrEgressDenied)
	assert.False(t, d.Allow, "a name resolving to the metadata IP is denied (DNS-rebind)")
}

// TestAllowlist_QUIC_Blocked verifies QUIC / non-DNS UDP is blocked: the
// proxy is TCP-CONNECT only, so any UDP flow is refused at the policy
// layer. Refs: SEC-04, MGIT-11.7.2
func TestAllowlist_QUIC_Blocked(t *testing.T) {
	aud := &fakeAuditor{}
	az := buildAuthorizer(t, []string{"registry.npmjs.org"}, resolvesTo("140.82.112.3"), aud)

	d, err := az.Authorize(context.Background(), Flow{Protocol: "udp", Host: "registry.npmjs.org", Port: 443})
	assert.ErrorIs(t, err, ErrEgressDenied, "UDP/QUIC is blocked (only TCP egress, DNS via the host resolver)")
	assert.False(t, d.Allow)
}

// TestAllowlist_EgressAudited verifies both an allow and a deny append an
// egress record (FR-17.8 — every allow and deny is audited). Refs: FR-17.8
func TestAllowlist_EgressAudited(t *testing.T) {
	aud := &fakeAuditor{}
	az := buildAuthorizer(t, []string{"registry.npmjs.org"}, resolvesTo("140.82.112.3"), aud)

	_, err := az.Authorize(context.Background(), Flow{Protocol: "tcp", Host: "registry.npmjs.org", Port: 443})
	require.NoError(t, err)
	_, err = az.Authorize(context.Background(), Flow{Protocol: "tcp", Host: "1.2.3.4", Port: 443})
	require.Error(t, err)

	var sawAllow, sawDeny bool
	for _, r := range aud.all() {
		if r.Protocol != "tcp" {
			continue
		}
		assert.Equal(t, "01SB", r.SandboxID)
		switch r.Decision {
		case model.EgressAllow:
			sawAllow = true
			assert.Equal(t, "140.82.112.3", r.DestIP)
		case model.EgressDeny:
			sawDeny = true
		}
	}
	assert.True(t, sawAllow, "the allowed flow is audited")
	assert.True(t, sawDeny, "the denied flow is audited")
}
