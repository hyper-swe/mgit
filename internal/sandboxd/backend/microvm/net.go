package microvm

import (
	"crypto/sha256"
	"fmt"
)

// GuestMAC derives a stable, locally-administered unicast MAC for a
// sandbox's guest NIC from its host-assigned sandbox ID.
//
// It lives here, on the shared backend seam, because the derivation is
// hypervisor-independent and security-relevant: the link address is
// host-assigned and never guest-chosen (SEC-05), and it must stay
// collision-free across every backend. A per-backend copy would be free to
// drift. Refs: FR-17.7, SEC-05
func GuestMAC(sandboxID string) string {
	sum := sha256.Sum256([]byte(sandboxID))
	// 0x02 = locally administered, unicast (low two bits of the first octet).
	return fmt.Sprintf("02:%02x:%02x:%02x:%02x:%02x", sum[0], sum[1], sum[2], sum[3], sum[4])
}
