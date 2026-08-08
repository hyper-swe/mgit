package guestboot

import (
	"net"
	"strconv"
	"strings"
)

// Kernel cmdline / env keys for the guest's own NIC configuration. The host
// composes them from the addresses IT already owns (the backend's gateway
// definition); the guest never restates an address.
const (
	// KeyNetIP is the guest's IPv4 address on the virtual link.
	KeyNetIP = "mgit.net_ip"
	// KeyNetPrefix is that address's prefix length (e.g. 24), which must put
	// the guest and the gateway on ONE link — a /32 would leave the gateway
	// off-link and inbound (SEC-09) connections unroutable.
	KeyNetPrefix = "mgit.net_prefix"
	// KeyNetGateway is the guest's default gateway: the host end of the wire,
	// and the only address the guest can reach directly.
	KeyNetGateway = "mgit.net_gw"
	// KeyNetDNS is the nameserver the guest writes into /etc/resolv.conf. It
	// is normally the gateway, where the host-side pinning resolver listens
	// (SEC-07); absent, the guest falls back to the gateway.
	KeyNetDNS = "mgit.net_dns"
)

// GuestNetwork is the host-supplied descriptor for the guest's own NIC
// (MGIT-68). Without it the guest boots with eth0 present but UNADDRESSED and
// with no default route — which fails every flow, allowed or denied, and is
// indistinguishable from working egress enforcement unless a test asserts
// that an ALLOWED flow succeeds.
//
// Why the guest is told rather than asked (the addressing decision, recorded):
// mgit-guest is PID 1, so no distro init, dhclient or NetworkManager ever
// runs in the sandbox — there is nothing for a static configuration to fight
// with. Serving DHCP instead would mean writing a DHCP server into the
// host-side gateway (gvisor's netstack ships none) AND a client into the
// guest, i.e. new code on both ends of a security boundary, to solve a
// problem the already-established boot-token channel solves with a parse. The
// host holds one definition of the link and hands the guest its half of it.
//
// The addresses live wherever the backend defines its gateway (libkrun:
// netgw.go); this descriptor is the transport, never a second definition.
// Refs: MGIT-68, FR-17.7, FR-17.8, SEC-07, ADR-010
type GuestNetwork struct {
	IP        string // guest IPv4 address on the virtual link
	PrefixLen int    // prefix length of the link (1..32)
	Gateway   string // default gateway (the host end of the wire)
	DNS       string // nameserver for /etc/resolv.conf; empty = the gateway
}

// Empty reports whether no network descriptor was supplied at all — a
// deliberately network-less sandbox (none mode), distinct from a partially
// specified, invalid one.
func (n GuestNetwork) Empty() bool {
	return n.IP == "" && n.PrefixLen == 0 && n.Gateway == "" && n.DNS == ""
}

// Valid reports whether the descriptor fully and sanely specifies an IPv4
// link. IPv6 is rejected because the backends' gateways serve IPv4 only, so
// an IPv6 descriptor could only produce a guest that configures a NIC and
// still reaches nothing. A guest with an invalid (but non-empty) descriptor
// must fail closed rather than half-configure. Refs: MGIT-68, FR-17.7
func (n GuestNetwork) Valid() bool {
	if n.PrefixLen < 1 || n.PrefixLen > 32 {
		return false
	}
	if !isIPv4(n.IP) || !isIPv4(n.Gateway) {
		return false
	}
	return n.DNS == "" || isIPv4(n.DNS)
}

// Resolver is the nameserver the guest must use: the explicit DNS address
// when given, otherwise the gateway — where the host-side allowlist-gated,
// address-pinning resolver listens (SEC-07). Defaulting matters: a guest with
// a route but no resolver still fails every name with EAI_AGAIN, which was
// half of the reported MGIT-68 symptom. Refs: SEC-07, MGIT-68
func (n GuestNetwork) Resolver() string {
	if n.DNS != "" {
		return n.DNS
	}
	return n.Gateway
}

// Netmask is the dotted-quad form of PrefixLen, which the guest's
// SIOCSIFNETMASK ioctl takes. Derived rather than carried, so the mask and
// the prefix cannot disagree. It is nil for an invalid descriptor.
func (n GuestNetwork) Netmask() net.IP {
	if !n.Valid() {
		return nil
	}
	return net.IP(net.CIDRMask(n.PrefixLen, 32))
}

// isIPv4 reports whether s parses as an IPv4 address.
func isIPv4(s string) bool {
	ip := net.ParseIP(s)
	return ip != nil && ip.To4() != nil
}

// AppendNetworkCmdline returns base with the guest network descriptor
// appended as space-separated key=value pairs. A descriptor with no address
// adds nothing — a none-mode sandbox gets no tokens, so its guest attempts no
// configuration at all. Refs: MGIT-68, FR-17.7
func AppendNetworkCmdline(base string, n GuestNetwork) string {
	if n.IP == "" {
		return base
	}
	parts := []string{
		KeyNetIP + "=" + n.IP,
		KeyNetPrefix + "=" + strconv.Itoa(n.PrefixLen),
		KeyNetGateway + "=" + n.Gateway,
	}
	if n.DNS != "" {
		parts = append(parts, KeyNetDNS+"="+n.DNS)
	}
	suffix := strings.Join(parts, " ")
	if strings.TrimSpace(base) == "" {
		return suffix
	}
	return base + " " + suffix
}

// ParseGuestNetwork extracts the guest network descriptor from a boot-token
// stream, ignoring unrelated tokens. A malformed prefix is left at zero,
// which Valid rejects — so a mangled descriptor fails closed instead of
// configuring a NIC from part of one. Refs: MGIT-68
func ParseGuestNetwork(tokens string) GuestNetwork {
	var n GuestNetwork
	for _, field := range strings.Fields(tokens) {
		key, value, ok := strings.Cut(field, "=")
		if !ok || value == "" {
			continue
		}
		switch key {
		case KeyNetIP:
			n.IP = value
		case KeyNetPrefix:
			if p, err := strconv.Atoi(value); err == nil && p >= 1 && p <= 32 {
				n.PrefixLen = p
			}
		case KeyNetGateway:
			n.Gateway = value
		case KeyNetDNS:
			n.DNS = value
		}
	}
	return n
}
