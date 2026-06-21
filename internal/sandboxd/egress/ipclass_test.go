package egress

import (
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestUnconditionalDeny_DeniesInternalRanges verifies the SEC-04 / T9
// unconditional denials: loopback, RFC1918 private, link-local, the cloud
// metadata IP, unique-local IPv6, multicast, and the unspecified address
// are refused regardless of any allowlist entry. Refs: SEC-04, FR-17.8
func TestUnconditionalDeny_DeniesInternalRanges(t *testing.T) {
	denied := []string{
		"127.0.0.1", "127.0.0.53", "::1", // loopback
		"10.0.0.1", "10.255.255.255", // RFC1918 10/8
		"172.16.0.1", "172.31.255.254", // RFC1918 172.16/12
		"192.168.0.1", "192.168.1.18", // RFC1918 192.168/16
		"169.254.0.1", "169.254.169.254", // link-local + cloud metadata (T9)
		"fe80::1",                 // IPv6 link-local
		"fc00::1", "fd12:3456::1", // IPv6 unique-local
		"100.64.0.1",           // RFC6598 carrier-grade NAT (shared, LAN-adjacent)
		"224.0.0.1", "ff02::1", // multicast
		"0.0.0.0", "::", // unspecified
		"255.255.255.255", // broadcast
	}
	for _, s := range denied {
		t.Run(s, func(t *testing.T) {
			ip := netip.MustParseAddr(s)
			reason, ok := IsUnconditionallyDenied(ip)
			assert.True(t, ok, "%s must be unconditionally denied (SEC-04/T9)", s)
			assert.NotEmpty(t, reason, "a deny must carry an audit reason")
		})
	}
}

// TestUnconditionalDeny_AllowsPublicIPs verifies routable public addresses
// are not unconditionally denied (the allowlist still governs them).
// Refs: SEC-04, FR-17.8
func TestUnconditionalDeny_AllowsPublicIPs(t *testing.T) {
	public := []string{
		"140.82.112.3",        // github.com
		"104.16.0.1",          // a CDN
		"8.8.8.8",             // public resolver
		"2606:50c0:8000::153", // public IPv6
	}
	for _, s := range public {
		t.Run(s, func(t *testing.T) {
			ip := netip.MustParseAddr(s)
			reason, ok := IsUnconditionallyDenied(ip)
			assert.False(t, ok, "%s is public; the allowlist governs it, not an unconditional deny", s)
			assert.Empty(t, reason)
		})
	}
}

// TestUnconditionalDeny_MetadataIPNamed pins the specific cloud metadata
// endpoint (169.254.169.254) as denied — the T9 lateral-movement /
// instance-credential-theft target. Refs: SEC-04, ADR-005 (T9)
func TestUnconditionalDeny_MetadataIPNamed(t *testing.T) {
	reason, ok := IsUnconditionallyDenied(netip.MustParseAddr("169.254.169.254"))
	assert.True(t, ok)
	assert.Contains(t, reason, "link-local", "the metadata IP is denied as link-local")
}

// TestUnconditionalDeny_EdgeCases covers the remaining classifications: an
// invalid address fails closed, and interface-local multicast is denied.
// Refs: SEC-04
func TestUnconditionalDeny_EdgeCases(t *testing.T) {
	reason, ok := IsUnconditionallyDenied(netip.Addr{})
	assert.True(t, ok, "the zero/invalid address fails closed")
	assert.Contains(t, reason, "invalid")

	reason, ok = IsUnconditionallyDenied(netip.MustParseAddr("ff01::1"))
	assert.True(t, ok, "interface-local multicast denied")
	assert.NotEmpty(t, reason)
}

// TestUnconditionalDeny_IPv4MappedV6 verifies an IPv4-mapped IPv6 address
// (::ffff:127.0.0.1) is unmapped before classification, so it cannot
// smuggle a denied IPv4 past the filter. Refs: SEC-04
func TestUnconditionalDeny_IPv4MappedV6(t *testing.T) {
	reason, ok := IsUnconditionallyDenied(netip.MustParseAddr("::ffff:169.254.169.254"))
	assert.True(t, ok, "IPv4-mapped metadata IP must be unmapped and denied")
	assert.NotEmpty(t, reason)
}
