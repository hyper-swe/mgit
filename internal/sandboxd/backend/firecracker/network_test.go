package firecracker

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
// invariants) at the fixed gateway ports the egress.Runner binds.
// Refs: SEC-04, MGIT-11.7.2
func TestTapPlanFor_UsesSharedEgressPlan(t *testing.T) {
	plan := tapPlanFor("01JABCDEF0123456789KLMNOPQ", "allowlist", "eth0")
	assert.Equal(t, egress.TapName("01JABCDEF0123456789KLMNOPQ"), plan.TapDev)
	assert.Equal(t, "allowlist", plan.Mode)
	assert.Equal(t, hostProxyPort, plan.ProxyPort)
	assert.Equal(t, hostDNSPort, plan.DNSPort)
	cmds, err := plan.SetupCommands()
	assert.NoError(t, err)
	assert.NotEmpty(t, cmds, "allowlist mode yields host network commands")
}

// fakeNetRunner records the commands it is asked to run, optionally failing
// when an argument contains a marker substring.
type fakeNetRunner struct {
	cmds   [][]string
	failOn string
}

func (f *fakeNetRunner) Run(_ context.Context, name string, args ...string) error {
	cmd := append([]string{name}, args...)
	f.cmds = append(f.cmds, cmd)
	if f.failOn != "" {
		for _, a := range cmd {
			if a == f.failOn {
				return errAtMarker
			}
		}
	}
	return nil
}

var errAtMarker = fmt.Errorf("runner failed at marker")

// TestApplyTapPlan_ExecsSetupInOrder verifies applyTapPlan runs exactly the
// plan's setup commands, in order. Refs: SEC-04, FR-17.7
func TestApplyTapPlan_ExecsSetupInOrder(t *testing.T) {
	plan := tapPlanFor("01JABCDEF0123456789KLMNOPQ", "allowlist", "eth0")
	want, err := plan.SetupCommands()
	require.NoError(t, err)

	runner := &fakeNetRunner{}
	require.NoError(t, applyTapPlan(context.Background(), runner, plan))
	assert.Equal(t, want, runner.cmds, "applyTapPlan execs the setup commands verbatim, in order")
}

// TestApplyTapPlan_NoneNoCommands verifies none mode runs nothing.
func TestApplyTapPlan_NoneNoCommands(t *testing.T) {
	runner := &fakeNetRunner{}
	require.NoError(t, applyTapPlan(context.Background(), runner, tapPlanFor("01SB", "none", "")))
	assert.Empty(t, runner.cmds)
}

// TestApplyTapPlan_FailClosed verifies a failed setup command aborts (fail
// closed — no half-applied policy fronting a guest). Refs: SEC-04
func TestApplyTapPlan_FailClosed(t *testing.T) {
	runner := &fakeNetRunner{failOn: "iptables"}
	err := applyTapPlan(context.Background(), runner, tapPlanFor("01SB", "allowlist", "eth0"))
	assert.Error(t, err, "a failed firewall command aborts setup")
}

// TestRemoveTapPlan_BestEffort verifies teardown attempts every command even
// when one fails (maximal residue removal). Refs: FR-17.19
func TestRemoveTapPlan_BestEffort(t *testing.T) {
	plan := tapPlanFor("01SB", "allowlist", "eth0")
	runner := &fakeNetRunner{failOn: "-F"} // fail mid-teardown
	removeTapPlan(context.Background(), runner, plan)
	assert.Equal(t, len(plan.TeardownCommands()), len(runner.cmds),
		"every teardown command is attempted despite a failure")
}
