package firecracker

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/hyper-swe/mgit/internal/sandboxd/egress"
)

// TestSubnetFor_DeterministicPointToPoint verifies each sandbox gets a
// stable /30 point-to-point link (gateway .1, guest .2) within the sandbox
// supernet, and distinct sandboxes get distinct links. Refs: FR-17.7
func TestSubnetFor_DeterministicPointToPoint(t *testing.T) {
	gw1, guest1, net1 := subnetFor("01JABCDEF0123456789KLMNOPQ")
	gw1b, guest1b, _ := subnetFor("01JABCDEF0123456789KLMNOPQ")
	_, guest2, _ := subnetFor("01JZZZZZZ0123456789KLMNOPQ")

	assert.Equal(t, gw1, gw1b, "deterministic gateway per sandbox")
	assert.Equal(t, guest1, guest1b, "deterministic guest IP per sandbox")
	assert.NotEqual(t, guest1, guest2, "distinct sandboxes get distinct guest IPs")

	assert.True(t, sandboxNetBase.Contains(gw1), "gateway within the sandbox supernet")
	assert.True(t, sandboxNetBase.Contains(guest1), "guest within the sandbox supernet")
	// gateway and guest are adjacent host addresses of the same /30.
	assert.Equal(t, gw1.Next(), guest1, "guest is gateway+1")
	assert.True(t, guest1.Is4(), "firecracker static IP config is IPv4-only")
	ones, _ := net1.Mask.Size()
	assert.Equal(t, 30, ones, "a /30 point-to-point link")
}

// TestGuestMAC_DeterministicLocallyAdministered verifies the guest MAC is
// stable per sandbox and uses a locally-administered unicast address.
// Refs: FR-17.7
func TestGuestMAC_DeterministicLocallyAdministered(t *testing.T) {
	m1 := guestMAC("01JABCDEF0123456789KLMNOPQ")
	m1b := guestMAC("01JABCDEF0123456789KLMNOPQ")
	m2 := guestMAC("01JZZZZZZ0123456789KLMNOPQ")
	assert.Equal(t, m1, m1b)
	assert.NotEqual(t, m1, m2)

	first := m1[:2]
	assert.True(t, strings.HasPrefix(m1, "02:"), "locally-administered, unicast (02 prefix), got %s", first)
	assert.Len(t, strings.Split(m1, ":"), 6, "six octets")
}

// TestTapPlanFor_UsesSharedEgressPlan verifies the backend builds its host
// tap plan from the shared egress package (one definition of the firewall
// invariants). Refs: SEC-04, MGIT-11.7.2
func TestTapPlanFor_UsesSharedEgressPlan(t *testing.T) {
	plan := tapPlanFor("01JABCDEF0123456789KLMNOPQ", "allowlist", 1080, 53, "eth0")
	assert.Equal(t, egress.TapName("01JABCDEF0123456789KLMNOPQ"), plan.TapDev)
	assert.Equal(t, "allowlist", plan.Mode)
	cmds, err := plan.SetupCommands()
	assert.NoError(t, err)
	assert.NotEmpty(t, cmds, "allowlist mode yields host network commands")
}
