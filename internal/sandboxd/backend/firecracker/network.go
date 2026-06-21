package firecracker

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"

	"github.com/hyper-swe/mgit/internal/sandboxd/egress"
)

// sandboxNetBase is the host-only supernet from which each sandbox gets its
// own /30 point-to-point link to the host tap. It is RFC1918 space — never a
// guest egress target (the egress proxy denies RFC1918); it exists solely as
// the L2 link between the guest NIC and the host gateway. Refs: FR-17.7
var sandboxNetBase = netip.MustParsePrefix("172.31.0.0/16")

// subnetCount is the number of /30 blocks in the /16 supernet (16384).
const subnetCount = 1 << 14

// subnetFor derives a sandbox's deterministic /30 point-to-point link: the
// host gateway (.1) and the guest (.2). Derivation is by hash of the sandbox
// ID, so a sandbox always maps to the same link without shared allocator
// state. The /16 yields 16384 links; collisions across simultaneously-live
// sandboxes are astronomically unlikely at the host's concurrency ceiling
// (FR-17.26). Refs: FR-17.7
func subnetFor(sandboxID string) (gateway, guest netip.Addr, guestNet net.IPNet) {
	sum := sha256.Sum256([]byte(sandboxID))
	block := binary.BigEndian.Uint32(sum[:4]) % subnetCount
	base := ipToU32(sandboxNetBase.Addr()) + block*4
	gateway = u32ToIP(base + 1)
	guest = u32ToIP(base + 2)
	guestNet = net.IPNet{IP: guest.AsSlice(), Mask: net.CIDRMask(30, 32)}
	return gateway, guest, guestNet
}

// guestMAC derives a stable, locally-administered (02:..) unicast MAC for a
// sandbox's guest NIC. Refs: FR-17.7
func guestMAC(sandboxID string) string {
	sum := sha256.Sum256([]byte(sandboxID))
	// 0x02 = locally administered, unicast (low two bits of the first octet).
	return fmt.Sprintf("02:%02x:%02x:%02x:%02x:%02x", sum[0], sum[1], sum[2], sum[3], sum[4])
}

// tapPlanFor builds the shared egress TapPlan for a sandbox's host tap,
// single-sourcing the per-mode firewall invariants from the egress package.
// Refs: SEC-04, FR-17.7, FR-17.8
func tapPlanFor(sandboxID, mode string, proxyPort, dnsPort int, extIface string) egress.TapPlan {
	gateway, guest, _ := subnetFor(sandboxID)
	return egress.TapPlan{
		Mode:      mode,
		TapDev:    egress.TapName(sandboxID),
		GuestIP:   guest,
		GatewayIP: gateway,
		ProxyPort: proxyPort,
		DNSPort:   dnsPort,
		ExtIface:  extIface,
	}
}

// ipToU32 converts an IPv4 netip.Addr to its uint32 value.
func ipToU32(a netip.Addr) uint32 {
	b := a.As4()
	return binary.BigEndian.Uint32(b[:])
}

// u32ToIP converts a uint32 to an IPv4 netip.Addr.
func u32ToIP(v uint32) netip.Addr {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	return netip.AddrFrom4(b)
}
