package egress

import (
	"context"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// buildAuthorizerWithDenial wires an authorizer whose deny path notifies the
// supplied observer (the deny->prompt seam).
func buildAuthorizerWithDenial(t *testing.T, entries []string, lookup LookupFunc, aud Auditor, onDenial func(model.ObservedDenial)) *Authorizer {
	t.Helper()
	al, err := Compile(entries)
	require.NoError(t, err)
	if lookup == nil {
		lookup = func(context.Context, string) ([]netip.Addr, error) { return nil, ErrNXDOMAIN }
	}
	res, err := NewResolver(ResolverConfig{
		SandboxID: "01SB", TaskID: "MGIT-11.9.4",
		Allowlist: al, Lookup: lookup, Audit: aud, Clock: frozenClock(),
	})
	require.NoError(t, err)
	az, err := NewAuthorizer(AuthorizerConfig{
		SandboxID: "01SB", TaskID: "MGIT-11.9.4",
		Allowlist: al, Resolver: res, Audit: aud, OnDenial: onDenial,
	})
	require.NoError(t, err)
	return az
}

// TestOnDenial_RawIPDenied_EscalatesHostObserved verifies a denial that names a
// concrete host-observed destination IP fires the escalation observer with ONLY
// host facts (sandbox, task, pinned IP, port) — the deny->prompt trigger.
// Refs: FR-17.12, SEC-05
func TestOnDenial_RawIPDenied_EscalatesHostObserved(t *testing.T) {
	var got []model.ObservedDenial
	az := buildAuthorizerWithDenial(t, []string{"registry.npmjs.org"}, resolvesTo("140.82.112.3"),
		&fakeAuditor{}, func(d model.ObservedDenial) { got = append(got, d) })

	_, err := az.Authorize(context.Background(), Flow{Protocol: "tcp", Host: "203.0.113.9", Port: 8443})
	require.ErrorIs(t, err, ErrEgressDenied)

	require.Len(t, got, 1, "a denial with a host-observed IP escalates")
	assert.Equal(t, "01SB", got[0].SandboxID)
	assert.Equal(t, "MGIT-11.9.4", got[0].TaskID)
	assert.Equal(t, netip.MustParseAddr("203.0.113.9"), got[0].DestIP)
	assert.Equal(t, 8443, got[0].DestPort)
}

// TestOnDenial_NoHostIP_NotEscalated verifies a denial with no concrete
// host-observed destination (a name that fails resolution) does NOT escalate —
// there is no IP to grant. Refs: FR-17.12
func TestOnDenial_NoHostIP_NotEscalated(t *testing.T) {
	var got []model.ObservedDenial
	// A name not on the allowlist is denied before any host resolution → no IP.
	az := buildAuthorizerWithDenial(t, []string{"registry.npmjs.org"}, resolvesTo("140.82.112.3"),
		&fakeAuditor{}, func(d model.ObservedDenial) { got = append(got, d) })

	_, err := az.Authorize(context.Background(), Flow{Protocol: "tcp", Host: "evil.example.com", Port: 443})
	require.ErrorIs(t, err, ErrEgressDenied)
	assert.Empty(t, got, "a denial without a host-observed IP is not escalatable")
}
