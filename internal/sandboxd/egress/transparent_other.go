//go:build !linux

package egress

import (
	"fmt"
	"net"
	"net/netip"
)

// OriginalDst reports that this platform has no redirect mechanism.
//
// The transparent redirect is the firecracker (Linux) allowlist path;
// libkrun's userspace netstack gateway learns the destination from the guest's
// own packets and needs none of this. Failing rather than guessing keeps the
// fail-closed rule: a proxy that cannot recover where a connection was headed
// must refuse it. Refs: MGIT-69, SEC-04
func OriginalDst(net.Conn) (netip.AddrPort, error) {
	return netip.AddrPort{}, fmt.Errorf(
		"original destination: SO_ORIGINAL_DST is Linux-only; this platform has no transparent redirect")
}
