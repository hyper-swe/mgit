// Package egress is the host-side network-policy enforcement for the
// microVM sandbox (FR-17.7, FR-17.8). It implements the allowlist egress
// proxy, the host-side restricted DNS resolver, and one-way guest->host
// port publishing. Every decision is made on HOST-OBSERVED facts — the
// resolved destination IP and the host's own DNS resolution — never on
// guest-supplied SNI or remedy text (SEC-04, SEC-05). The guest gets no
// direct route; this package is the only egress path, so a hostile guest
// cannot weaken its own policy.
package egress

import (
	"net/netip"
)

// metadataIP is the cloud instance-metadata endpoint (AWS/GCP/Azure/
// OpenStack all answer here). Reaching it from a sandbox is the classic
// T9 instance-credential theft; it is denied unconditionally even though
// it is already link-local, so the audit trail names it explicitly.
var metadataIP = netip.MustParseAddr("169.254.169.254")

// deniedPrefixes are reserved / special-use ranges denied unconditionally,
// beyond what the netip method checks (loopback, private, link-local,
// multicast, unspecified) already cover. Each is LAN-adjacent, non-routable,
// or a translation prefix that re-embeds a denied IPv4 — so a (mis)configured
// allowlist or a DNS64 answer must never reach them (SEC-04 / T9). The NAT64
// prefixes matter because a host with a NAT64 gateway translates
// 64:ff9b::<v4> to that IPv4: an allowlisted name resolving to
// 64:ff9b::169.254.169.254 would otherwise look like a public IPv6.
// Refs: SEC-04, ADR-005 (T9)
var deniedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"),   // RFC6598 carrier-grade NAT (LAN-adjacent)
	netip.MustParsePrefix("0.0.0.0/8"),       // "this network" (RFC1122)
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF protocol assignments (incl. NAT64 192.0.0.170/.171)
	netip.MustParsePrefix("192.0.2.0/24"),    // TEST-NET-1 documentation
	netip.MustParsePrefix("198.18.0.0/15"),   // benchmarking (RFC2544)
	netip.MustParsePrefix("198.51.100.0/24"), // TEST-NET-2 documentation
	netip.MustParsePrefix("203.0.113.0/24"),  // TEST-NET-3 documentation
	netip.MustParsePrefix("240.0.0.0/4"),     // reserved (class E), incl. 255.255.255.255 broadcast
	netip.MustParsePrefix("64:ff9b::/96"),    // NAT64 well-known (RFC6052) — re-embeds an IPv4
	netip.MustParsePrefix("64:ff9b:1::/48"),  // NAT64 local-use (RFC8215)
	netip.MustParsePrefix("2001:db8::/32"),   // IPv6 documentation
	netip.MustParsePrefix("100::/64"),        // IPv6 discard-only (RFC6666)
}

// IsUnconditionallyDenied reports whether an egress destination IP must be
// refused regardless of any allowlist entry, and an audit reason when so.
// These are the SEC-04 / T9 denials: a payload must not reach host loopback,
// the LAN (RFC1918 / unique-local / CGNAT), link-local space, the cloud
// metadata endpoint, or reserved / NAT64 ranges — even if a (mis)configured
// allowlist names them. An IPv4-mapped IPv6 address is unmapped first so a
// denied IPv4 cannot be smuggled in v6 clothing. Refs: SEC-04, FR-17.8, ADR-005 (T9)
func IsUnconditionallyDenied(ip netip.Addr) (string, bool) {
	ip = ip.Unmap()
	switch {
	case !ip.IsValid():
		return "invalid address", true
	case ip == metadataIP:
		return "cloud metadata endpoint (link-local, T9)", true
	case ip.IsLoopback():
		return "loopback", true
	case ip.IsUnspecified():
		return "unspecified address", true
	case ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast():
		return "link-local", true
	case ip.IsMulticast():
		return "multicast", true
	case ip.IsInterfaceLocalMulticast():
		return "interface-local multicast", true
	case ip.IsPrivate():
		// RFC1918 (10/8, 172.16/12, 192.168/16) and IPv6 ULA (fc00::/7).
		return "private (RFC1918/ULA)", true
	}
	for _, p := range deniedPrefixes {
		if p.Contains(ip) {
			return "reserved/special-use range " + p.String(), true
		}
	}
	return "", false
}
