package egress

import (
	"context"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAllowlist_GrantIP_AdmitsExactDestOnly proves a live capability grant
// widens the allowlist for exactly one (ip, port) — never a range — and that
// the launch-time entries are unaffected. This is the live-enforcement half
// of the SEC-05 scoped grant.
func TestAllowlist_GrantIP_AdmitsExactDestOnly(t *testing.T) {
	t.Parallel()

	al, err := Compile([]string{"registry.npmjs.org:443"})
	require.NoError(t, err)

	dest := netip.MustParseAddr("198.51.100.9")
	other := netip.MustParseAddr("198.51.100.10")

	assert.False(t, al.AllowsIP(dest, 443), "denied before grant")

	require.NoError(t, al.GrantIP(dest, 443))
	assert.True(t, al.AllowsIP(dest, 443), "admitted after grant")
	assert.False(t, al.AllowsIP(dest, 80), "grant is port-scoped")
	assert.False(t, al.AllowsIP(other, 443), "grant is host-scoped (no range)")

	al.RevokeGrants()
	assert.False(t, al.AllowsIP(dest, 443), "revoked on teardown")
}

// TestAllowlist_GrantIP_RejectsDeniedRange is a defense-in-depth check: even a
// granted IP in an unconditionally-denied range is still refused by the
// authorizer's denied-range gate (the grant cannot re-open link-local/loopback
// lateral movement). Refs: SEC-04
func TestAllowlist_GrantIP_RejectsDeniedRange(t *testing.T) {
	t.Parallel()

	linkLocal := netip.MustParseAddr("169.254.169.254") // cloud metadata
	aud := &fakeAuditor{}
	az := buildAuthorizer(t, []string{"registry.npmjs.org"}, resolvesTo("140.82.112.3"), aud)
	require.NoError(t, az.cfg.Allowlist.GrantIP(linkLocal, 443))

	dec, err := az.Authorize(context.Background(), Flow{Protocol: "tcp", Host: linkLocal.String(), Port: 443})
	require.Error(t, err, "denied range beats a grant")
	assert.False(t, dec.Allow)
}

// TestRunner_AllowEgress_WidensLiveSandbox proves the Runner exposes the
// EgressGranter contract: AllowEgress widens the named live sandbox's
// allowlist, and RevokeAll drops every grant for that sandbox. Refs: FR-17.12
func TestRunner_AllowEgress_WidensLiveSandbox(t *testing.T) {
	t.Parallel()

	rec := &dialRecorder{}
	r := testRunner(t, rec)
	_, err := r.Start(context.Background(), Binding{
		SandboxID: "01SB", TaskID: "MGIT-11.9.4", GatewayIP: netip.MustParseAddr("127.0.0.1"),
		Policy: allowlistPolicy("registry.npmjs.org"),
	})
	require.NoError(t, err)
	defer func() { _ = r.Stop("01SB") }()

	require.NoError(t, r.AllowEgress(context.Background(), "01SB", "198.51.100.9:443"))
	al, ok := r.allowlistFor("01SB")
	require.True(t, ok)
	assert.True(t, al.AllowsIP(netip.MustParseAddr("198.51.100.9"), 443))

	r.RevokeAll("01SB")
	al2, ok := r.allowlistFor("01SB")
	require.True(t, ok, "the sandbox is still running; only its grants are dropped")
	assert.False(t, al2.AllowsIP(netip.MustParseAddr("198.51.100.9"), 443))
}

// TestAllowlist_GrantIP_RejectsBadInput proves GrantIP validates its inputs:
// an invalid IP or out-of-range port is refused, never silently admitted.
func TestAllowlist_GrantIP_RejectsBadInput(t *testing.T) {
	t.Parallel()
	al, err := Compile([]string{"registry.npmjs.org"})
	require.NoError(t, err)
	assert.Error(t, al.GrantIP(netip.Addr{}, 443), "invalid ip refused")
	assert.Error(t, al.GrantIP(netip.MustParseAddr("198.51.100.9"), 0), "invalid port refused")
	assert.Error(t, al.GrantIP(netip.MustParseAddr("198.51.100.9"), 70000), "port too large refused")
}

// TestRunner_allowlistFor_UnknownSandbox proves the live-allowlist lookup
// reports absence for a sandbox with no running egress stack.
func TestRunner_allowlistFor_UnknownSandbox(t *testing.T) {
	t.Parallel()
	r := testRunner(t, &dialRecorder{})
	_, ok := r.allowlistFor("nope")
	assert.False(t, ok)
}

// TestRunner_AllowEgress_NoColonEntry proves an entry with no port separator
// is rejected.
func TestRunner_AllowEgress_NoColonEntry(t *testing.T) {
	t.Parallel()
	r := testRunner(t, &dialRecorder{})
	_, err := r.Start(context.Background(), Binding{
		SandboxID: "01SB", TaskID: "MGIT-11.9.4", GatewayIP: netip.MustParseAddr("127.0.0.1"),
		Policy: allowlistPolicy("registry.npmjs.org"),
	})
	require.NoError(t, err)
	defer func() { _ = r.Stop("01SB") }()
	assert.Error(t, r.AllowEgress(context.Background(), "01SB", "198.51.100.9"))
}

// TestRunner_AllowEgress_UnknownSandbox_Error proves a grant for a sandbox
// with no running egress stack is refused (fail closed). Refs: FR-17.12
func TestRunner_AllowEgress_UnknownSandbox_Error(t *testing.T) {
	t.Parallel()
	r := testRunner(t, &dialRecorder{})
	err := r.AllowEgress(context.Background(), "sbx-missing", "198.51.100.9:443")
	require.Error(t, err)
}

// TestRunner_AllowEgress_BadEntry_Error proves a malformed grant entry is
// rejected rather than silently dropped. Refs: FR-17.12
func TestRunner_AllowEgress_BadEntry_Error(t *testing.T) {
	t.Parallel()
	r := testRunner(t, &dialRecorder{})
	_, err := r.Start(context.Background(), Binding{
		SandboxID: "01SB", TaskID: "MGIT-11.9.4", GatewayIP: netip.MustParseAddr("127.0.0.1"),
		Policy: allowlistPolicy("registry.npmjs.org"),
	})
	require.NoError(t, err)
	defer func() { _ = r.Stop("01SB") }()

	for _, bad := range []string{"not-an-ip:443", "198.51.100.9", "198.51.100.9:0", "198.51.100.9:99999"} {
		err := r.AllowEgress(context.Background(), "01SB", bad)
		require.Errorf(t, err, "entry %q must be rejected", bad)
	}
}
