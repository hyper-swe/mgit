package libkrun

import (
	"net"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/guestboot"
	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/backend/microvm"
)

// bootTokensFrom extracts the boot-token string from a guest environment.
func bootTokensFrom(t *testing.T, env []string) string {
	t.Helper()
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, guestboot.EnvBootTokens+"="); ok {
			return v
		}
	}
	return ""
}

// TestGuestEnv_NetworkedModes_CarryTheAddressing is the host half of the
// MGIT-68 fix: a sandbox with a gateway must TELL its guest the address,
// prefix, gateway and resolver. Without these tokens mgit-guest — which is
// PID 1, so nothing else configures anything — leaves eth0 unaddressed and
// every flow fails, allowed or denied. Refs: MGIT-68, FR-17.7, SEC-07
func TestGuestEnv_NetworkedModes_CarryTheAddressing(t *testing.T) {
	for _, mode := range []string{model.NetworkModeAllowlist, model.NetworkModeOpen} {
		t.Run(mode, func(t *testing.T) {
			env := guestEnv(microvm.VMConfig{SandboxID: "s1", NetworkMode: mode})

			got := guestboot.ParseGuestNetwork(bootTokensFrom(t, env))

			require.True(t, got.Valid(), "guest network descriptor must be complete, got %+v", got)
			// SINGLE SOURCE OF TRUTH: the values are asserted against the
			// gateway's own constants, not against literals. A second copy of
			// an address is how this breaks again.
			assert.Equal(t, guestIP, got.IP)
			assert.Equal(t, gwPrefixLen, got.PrefixLen)
			assert.Equal(t, gatewayIP, got.Gateway)
			assert.Equal(t, gatewayIP, got.Resolver(),
				"the resolver is the gateway: that is where the pinning DNS server listens (SEC-07)")
		})
	}
}

// TestGuestEnv_NoneMode_CarriesNoAddressing verifies a network-less sandbox
// gets no descriptor. Its NIC exists only to keep libkrun off its TSI
// fallback and is backed by a discard socket, so handing the guest an address
// would dress a dead network as a live one. Refs: FR-17.7, ADR-010
func TestGuestEnv_NoneMode_CarriesNoAddressing(t *testing.T) {
	env := guestEnv(microvm.VMConfig{SandboxID: "s1", NetworkMode: model.NetworkModeNone})

	assert.True(t, guestboot.ParseGuestNetwork(bootTokensFrom(t, env)).Empty())
}

// TestGuestEnv_AddressingTravelsWithTheOtherDescriptors verifies the network
// tokens coexist with the worktree and published-ports descriptors on the one
// env channel libkrun has. Refs: MGIT-68, FR-17.3, SEC-09, ADR-010
func TestGuestEnv_AddressingTravelsWithTheOtherDescriptors(t *testing.T) {
	env := guestEnv(microvm.VMConfig{
		SandboxID:    "s1",
		NetworkMode:  model.NetworkModeAllowlist,
		WorktreePath: "/home/dev/wt",
		WorktreeTag:  "work",
		PublishPorts: []int{3000},
	})

	tokens := bootTokensFrom(t, env)
	assert.True(t, guestboot.ParseGuestNetwork(tokens).Valid())
	assert.Equal(t, "/home/dev/wt", guestboot.ParseWorktreeMount(tokens).Path)
	assert.Equal(t, []int{3000}, guestboot.ParsePublishPorts(tokens))
}

// TestGuestEnv_NoWorktree_StillCarriesAddressing verifies a sandbox with no
// worktree still gets its network. The env channel was previously set ONLY
// when a worktree descriptor existed, so this pins that the network tokens do
// not inherit that condition. Refs: MGIT-68
func TestGuestEnv_NoWorktree_StillCarriesAddressing(t *testing.T) {
	env := guestEnv(microvm.VMConfig{SandboxID: "s1", NetworkMode: model.NetworkModeOpen})

	assert.True(t, guestboot.ParseGuestNetwork(bootTokensFrom(t, env)).Valid(),
		"a worktree-less sandbox still needs a route out")
}

// TestGuestNetworkForMode_MatchesTheGatewayLink verifies the descriptor the
// host composes puts the guest and the gateway on ONE link — the property
// DialGuestPort depends on for inbound (SEC-09) connections.
func TestGuestNetworkForMode_MatchesTheGatewayLink(t *testing.T) {
	n := guestNetworkFor(model.NetworkModeAllowlist)
	require.True(t, n.Valid())

	_, link, err := net.ParseCIDR(n.IP + "/" + strconv.Itoa(n.PrefixLen))
	require.NoError(t, err)
	assert.True(t, link.Contains(net.ParseIP(gatewayIP)),
		"the gateway must be on the guest's link, or inbound publishing is unroutable (SEC-09)")
}
