package egress

import (
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCompile_RejectsMatchAllAndGarbage(t *testing.T) {
	for _, entry := range []string{"*", "*.", "not a host", ""} {
		t.Run(entry, func(t *testing.T) {
			_, err := Compile([]string{entry})
			assert.Error(t, err, "match-all / malformed entry %q must be rejected (no allow-all, SEC-04)", entry)
		})
	}
}

func TestAllowlist_NameMatching(t *testing.T) {
	al, err := Compile([]string{"registry.npmjs.org", "*.golang.org", "pypi.org:443"})
	require.NoError(t, err)

	// exact name, any port
	assert.True(t, al.AllowsName("registry.npmjs.org", 443))
	assert.True(t, al.AllowsName("registry.npmjs.org", 80))
	// exact name not listed
	assert.False(t, al.AllowsName("evil.npmjs.org", 443))
	// wildcard matches subdomains, not the bare apex
	assert.True(t, al.AllowsName("proxy.golang.org", 443))
	assert.True(t, al.AllowsName("a.b.golang.org", 443))
	assert.False(t, al.AllowsName("golang.org", 443), "*.golang.org does not match the apex")
	assert.False(t, al.AllowsName("notgolang.org", 443))
	// port-constrained entry
	assert.True(t, al.AllowsName("pypi.org", 443))
	assert.False(t, al.AllowsName("pypi.org", 22), "pypi.org:443 forbids other ports")
	// case-insensitive host input (the guest may upper-case)
	assert.True(t, al.AllowsName("REGISTRY.NPMJS.ORG", 443))
}

func TestAllowlist_IPAndCIDRMatching(t *testing.T) {
	al, err := Compile([]string{"140.82.112.3", "104.16.0.0/16", "8.8.8.8:53"})
	require.NoError(t, err)

	assert.True(t, al.AllowsIP(netip.MustParseAddr("140.82.112.3"), 443))
	assert.False(t, al.AllowsIP(netip.MustParseAddr("140.82.112.4"), 443))
	// CIDR membership
	assert.True(t, al.AllowsIP(netip.MustParseAddr("104.16.5.9"), 443))
	assert.False(t, al.AllowsIP(netip.MustParseAddr("104.17.0.1"), 443))
	// port-constrained IP
	assert.True(t, al.AllowsIP(netip.MustParseAddr("8.8.8.8"), 53))
	assert.False(t, al.AllowsIP(netip.MustParseAddr("8.8.8.8"), 443))
	// a plain hostname entry does NOT authorize a raw-IP connection
	assert.False(t, al.AllowsIP(netip.MustParseAddr("140.82.112.5"), 443))
}

func TestAllowlist_HasNameForResolver(t *testing.T) {
	al, err := Compile([]string{"registry.npmjs.org", "*.golang.org", "8.8.8.8"})
	require.NoError(t, err)

	// the resolver resolves only allowlisted names
	assert.True(t, al.HasName("registry.npmjs.org"))
	assert.True(t, al.HasName("proxy.golang.org"))
	assert.False(t, al.HasName("evil.com"))
	// an IP-only allowlist authorizes no names
	assert.False(t, al.HasName("8.8.8.8"))
}

func TestCompile_InvalidCIDRAndPort(t *testing.T) {
	for _, entry := range []string{"10.0.0.0/99", "pypi.org:0", "pypi.org:99999", "pypi.org:1:2"} {
		t.Run(entry, func(t *testing.T) {
			_, err := Compile([]string{entry})
			assert.Error(t, err, "invalid CIDR/port entry %q must be rejected", entry)
		})
	}
}

func TestAllowlist_EmptyAllowsNothing(t *testing.T) {
	al, err := Compile(nil)
	require.NoError(t, err)
	assert.False(t, al.AllowsName("registry.npmjs.org", 443))
	assert.False(t, al.AllowsIP(netip.MustParseAddr("8.8.8.8"), 53))
	assert.False(t, al.HasName("anything"))
}
