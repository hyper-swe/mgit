package egress

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

func allowlistPlan() TapPlan {
	return TapPlan{
		Mode:      model.NetworkModeAllowlist,
		TapDev:    "mgt0a1b2c3d",
		GuestIP:   netip.MustParseAddr("172.31.0.2"),
		GatewayIP: netip.MustParseAddr("172.31.0.1"),
		ProxyPort: 1080,
		DNSPort:   53,
		ExtIface:  "eth0",
	}
}

func flatten(cmds [][]string) string {
	var b strings.Builder
	for _, c := range cmds {
		b.WriteString(strings.Join(c, " "))
		b.WriteByte('\n')
	}
	return b.String()
}

// TestTapName_DeterministicAndBounded verifies the host tap name is derived
// from the sandbox ID, stable, and within the 15-byte interface-name limit.
// Refs: FR-17.7
func TestTapName_DeterministicAndBounded(t *testing.T) {
	a := TapName("01JABCDEF0123456789KLMNOPQ")
	b := TapName("01JABCDEF0123456789KLMNOPQ")
	c := TapName("01JZZZZZZ0123456789KLMNOPQ")
	assert.Equal(t, a, b, "deterministic for one sandbox")
	assert.NotEqual(t, a, c, "distinct sandboxes get distinct taps")
	assert.LessOrEqual(t, len(a), 15, "Linux IFNAMSIZ is 15 bytes")
	assert.True(t, strings.HasPrefix(a, "mgt"), "namespaced prefix")
}

// TestTapPlan_AllowlistDefaultDenyNoNAT verifies the allowlist ruleset gives
// the guest NO direct route: it permits only the proxy and the host DNS
// resolver and DROPs everything else, with NO masquerade/NAT (the proxy,
// not the kernel, reaches the internet). This is what makes allowlist
// host-enforced and guest-unweakenable. Refs: SEC-04, FR-17.8
func TestTapPlan_AllowlistDefaultDenyNoNAT(t *testing.T) {
	cmds, err := allowlistPlan().SetupCommands()
	require.NoError(t, err)
	out := flatten(cmds)

	assert.NotContains(t, out, "MASQUERADE", "allowlist must NOT NAT the guest to the internet (no direct route)")
	assert.Contains(t, out, "172.31.0.1", "guest egress is steered to the host gateway (proxy + resolver)")
	assert.Contains(t, out, "1080", "the egress proxy port is reachable")
	assert.Contains(t, out, "DROP", "default-deny: everything else from the guest is dropped")
	// the proxy ACCEPT rule must precede the DROP (order matters in iptables)
	assert.Less(t, strings.Index(out, "1080"), strings.LastIndex(out, "DROP"),
		"the proxy ACCEPT must come before the default DROP")
}

// TestTapPlan_AllowlistUsesPrivateChainAtTop verifies the rules live in a
// dedicated per-sandbox chain jumped to from the TOP (-I ... 1) of INPUT and
// FORWARD, so they take precedence over any pre-existing host ACCEPT (e.g.
// Docker/libvirt) and cannot be bypassed. Refs: SEC-04 (review finding 2)
func TestTapPlan_AllowlistUsesPrivateChainAtTop(t *testing.T) {
	plan := allowlistPlan()
	cmds, err := plan.SetupCommands()
	require.NoError(t, err)
	out := flatten(cmds)
	chain := "f" + plan.TapDev

	assert.Contains(t, out, "-N "+chain, "a private per-sandbox chain is created")
	assert.Contains(t, out, "-I INPUT 1 -i "+plan.TapDev+" -j "+chain, "INPUT jump inserted at the top")
	assert.Contains(t, out, "-I FORWARD 1 -i "+plan.TapDev+" -j "+chain, "FORWARD jump inserted at the top")

	// teardown removes the jumps and deletes the chain (no stale rules).
	tout := flatten(plan.TeardownCommands())
	assert.Contains(t, tout, "-D INPUT -i "+plan.TapDev+" -j "+chain, "INPUT jump removed")
	assert.Contains(t, tout, "-D FORWARD -i "+plan.TapDev+" -j "+chain, "FORWARD jump removed")
	assert.Contains(t, tout, "-X "+chain, "the chain is deleted (no residue)")
}

// TestTapPlan_OpenMasquerades verifies open mode NATs the guest to the host
// network (full egress) — the explicitly risky posture. Refs: FR-17.7
func TestTapPlan_OpenMasquerades(t *testing.T) {
	plan := allowlistPlan()
	plan.Mode = model.NetworkModeOpen
	cmds, err := plan.SetupCommands()
	require.NoError(t, err)
	out := flatten(cmds)

	assert.Contains(t, out, "MASQUERADE", "open mode NATs the guest to the host network")
	assert.Contains(t, out, "eth0", "masquerade on the external interface")
}

// TestTapPlan_NoneHasNoRules verifies none mode produces no host network
// rules (there is no NIC/tap to govern). Refs: FR-17.7
func TestTapPlan_NoneHasNoRules(t *testing.T) {
	plan := allowlistPlan()
	plan.Mode = model.NetworkModeNone
	cmds, err := plan.SetupCommands()
	require.NoError(t, err)
	assert.Empty(t, cmds, "none mode attaches no NIC, so there are no host network rules")
}

// TestTapPlan_TeardownRemovesEverything verifies teardown deletes the tap so
// no host residue remains (FR-17.19). Refs: FR-17.19
func TestTapPlan_TeardownRemovesEverything(t *testing.T) {
	out := flatten(allowlistPlan().TeardownCommands())
	assert.Contains(t, out, "mgt0a1b2c3d", "the tap device is deleted at teardown")
	assert.Contains(t, out, "del", "teardown removes, never adds")
}

// TestTapPlan_OpenTeardownRemovesNAT verifies open-mode teardown removes the
// nat-table masquerade (not interface-scoped) plus the tap. Refs: FR-17.19
func TestTapPlan_OpenTeardownRemovesNAT(t *testing.T) {
	plan := allowlistPlan()
	plan.Mode = model.NetworkModeOpen
	out := flatten(plan.TeardownCommands())
	assert.Contains(t, out, "MASQUERADE", "the nat masquerade is explicitly deleted")
	assert.Contains(t, out, "link del", "the tap is deleted")
}

// TestTapPlan_NoneTeardownEmpty verifies none mode has nothing to tear down.
func TestTapPlan_NoneTeardownEmpty(t *testing.T) {
	plan := allowlistPlan()
	plan.Mode = model.NetworkModeNone
	assert.Empty(t, plan.TeardownCommands())
}

// TestTapPlan_UnknownMode_Errors fails closed on an unrecognized mode.
func TestTapPlan_UnknownMode_Errors(t *testing.T) {
	plan := allowlistPlan()
	plan.Mode = "bogus"
	_, err := plan.SetupCommands()
	assert.Error(t, err)
}

// TestTapPlan_ValidateBranches covers the remaining fail-closed guards.
func TestTapPlan_ValidateBranches(t *testing.T) {
	t.Run("open_needs_ext_iface", func(t *testing.T) {
		plan := allowlistPlan()
		plan.Mode = model.NetworkModeOpen
		plan.ExtIface = ""
		_, err := plan.SetupCommands()
		assert.Error(t, err)
	})
	t.Run("allowlist_needs_dns_port", func(t *testing.T) {
		plan := allowlistPlan()
		plan.DNSPort = 0
		_, err := plan.SetupCommands()
		assert.Error(t, err)
	})
	t.Run("invalid_ips", func(t *testing.T) {
		plan := allowlistPlan()
		plan.GuestIP = netip.Addr{}
		_, err := plan.SetupCommands()
		assert.Error(t, err)
	})
}

// TestTapPlan_Validate rejects an incomplete plan (fail closed).
func TestTapPlan_Validate(t *testing.T) {
	bad := allowlistPlan()
	bad.TapDev = ""
	_, err := bad.SetupCommands()
	assert.Error(t, err)

	bad = allowlistPlan()
	bad.ProxyPort = 0
	_, err = bad.SetupCommands()
	assert.Error(t, err, "allowlist mode needs a proxy port")
}
