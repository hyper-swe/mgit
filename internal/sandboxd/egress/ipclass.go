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

// cgnatPrefix is RFC6598 carrier-grade-NAT space (100.64.0.0/10): not
// RFC1918, but shared LAN-adjacent address space that must never be a
// sandbox egress target (T9 lateral movement).
var cgnatPrefix = netip.MustParsePrefix("100.64.0.0/10")

// IsUnconditionallyDenied reports whether an egress destination IP must be
// refused regardless of any allowlist entry, and an audit reason when so.
// These are the SEC-04 / T9 denials: a payload must not reach host
// loopback, the LAN (RFC1918 / unique-local / CGNAT), link-local space, or
// the cloud metadata endpoint — even if a (mis)configured allowlist names
// them. An IPv4-mapped IPv6 address is unmapped first so a denied IPv4
// cannot be smuggled in v6 clothing. Refs: SEC-04, FR-17.8, ADR-005 (T9)
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
	case cgnatPrefix.Contains(ip):
		return "carrier-grade NAT (RFC6598)", true
	case ip.Is4() && ip == netip.AddrFrom4([4]byte{255, 255, 255, 255}):
		return "broadcast", true
	}
	return "", false
}
