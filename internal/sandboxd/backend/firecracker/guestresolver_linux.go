//go:build linux

package firecracker

import "github.com/hyper-swe/mgit/internal/guestboot"

// guestNetworkFor is what the GUEST must be told about its own networking:
// the resolver, and only the resolver.
//
// It is derived from the same subnetFor the SDK's IPConfiguration is built
// from, so the nameserver on the kernel command line and the one in the
// guest's /etc/resolv.conf cannot drift apart. It is deliberately
// RESOLVER-ONLY: the kernel's `ip=` autoconfiguration already applies the
// address, prefix and default route on this backend, and duplicating a
// working mechanism is how the two copies eventually disagree.
//
// The gateway is the right answer in every mode that has a NIC, because the
// host-side resolver binds there in both allowlist and open mode — allowlist
// gating only which names resolve, not where. Refs: MGIT-69, SEC-07, FR-17.8
func guestNetworkFor(sandboxID string) guestboot.GuestNetwork {
	return guestboot.GuestNetwork{DNS: GatewayFor(sandboxID).String()}
}
