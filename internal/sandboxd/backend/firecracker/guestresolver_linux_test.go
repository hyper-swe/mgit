//go:build linux

package firecracker

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/guestboot"
	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/backend/microvm"
)

// netCfg is a VMConfig with a NIC in the given mode.
func netCfg(mode string) microvm.VMConfig {
	return microvm.VMConfig{
		SandboxID:   "sbx-resolver",
		Cmdline:     "console=ttyS0 reboot=k",
		CPUs:        1,
		MemoryMB:    256,
		AttachNIC:   mode != model.NetworkModeNone,
		NetworkMode: mode,
	}
}

// TestBuildConfig_NetworkedModes_TellTheGuestItsResolver is the MGIT-69 host
// half. The guest kernel applies the SDK's `ip=` boot parameter, so it HAS an
// address and a route — but the kernel writes the nameservers to
// /proc/net/pnp, not /etc/resolv.conf, and the guest rootfs ships no /etc at
// all. So every getaddrinfo caller in the guest (npm, apt, curl) had no
// resolver. The descriptor mgit-guest already knows how to apply now carries
// it. Refs: MGIT-69, FR-17.8, SEC-07
func TestBuildConfig_NetworkedModes_TellTheGuestItsResolver(t *testing.T) {
	for _, mode := range []string{model.NetworkModeAllowlist, model.NetworkModeOpen} {
		t.Run(mode, func(t *testing.T) {
			cfg := netCfg(mode)

			out := buildConfig(cfg, vmPaths{}, "")

			got := guestboot.ParseGuestNetwork(out.KernelArgs)
			require.True(t, got.Valid(), "resolver descriptor must be coherent, got %+v", got)
			// SINGLE SOURCE OF TRUTH: asserted against the gateway the backend
			// itself derives, never a literal.
			assert.Equal(t, GatewayFor(cfg.SandboxID).String(), got.Resolver())
		})
	}
}

// TestBuildConfig_ResolverDescriptorIsResolverOnly verifies mgit does NOT
// send a second link configuration. The kernel's `ip=` already addresses the
// guest and that mechanism works; duplicating it would be a second way to do
// one thing, and the one that is missing is the resolver. Refs: MGIT-69
func TestBuildConfig_ResolverDescriptorIsResolverOnly(t *testing.T) {
	out := buildConfig(netCfg(model.NetworkModeAllowlist), vmPaths{}, "")

	got := guestboot.ParseGuestNetwork(out.KernelArgs)
	assert.False(t, got.ConfiguresLink(),
		"firecracker must not re-send link configuration; the kernel's ip= owns that")
	assert.Empty(t, got.IP)
	assert.Empty(t, got.Gateway)
	assert.Zero(t, got.PrefixLen)
}

// TestBuildConfig_ResolverMatchesTheNameserverTheSDKIsGiven pins the two
// against each other: the kernel boot parameter and the resolv.conf token
// must name the SAME address, or the guest would resolve through one and be
// filtered by the other. Refs: MGIT-69
func TestBuildConfig_ResolverMatchesTheNameserverTheSDKIsGiven(t *testing.T) {
	cfg := netCfg(model.NetworkModeAllowlist)

	out := buildConfig(cfg, vmPaths{}, "")

	require.Len(t, out.NetworkInterfaces, 1)
	ipConf := out.NetworkInterfaces[0].StaticConfiguration.IPConfiguration
	require.Len(t, ipConf.Nameservers, 1)
	assert.Equal(t, ipConf.Nameservers[0], guestboot.ParseGuestNetwork(out.KernelArgs).Resolver())
}

// TestBuildConfig_NoneMode_CarriesNoResolver verifies a network-less sandbox
// is told nothing: with no NIC there is no gateway to resolve through, and a
// resolv.conf pointing at an unreachable address would make a deliberately
// dead network look configured. Refs: FR-17.7
func TestBuildConfig_NoneMode_CarriesNoResolver(t *testing.T) {
	out := buildConfig(netCfg(model.NetworkModeNone), vmPaths{}, "")

	assert.True(t, guestboot.ParseGuestNetwork(out.KernelArgs).Empty())
	assert.Empty(t, out.NetworkInterfaces)
}

// TestBuildConfig_ResolverTokenCoexistsWithTheOtherDescriptors verifies the
// resolver token does not displace the worktree, overlay or publish-port
// descriptors on the shared kernel command line. Refs: FR-17.3, SEC-09
func TestBuildConfig_ResolverTokenCoexistsWithTheOtherDescriptors(t *testing.T) {
	cfg := netCfg(model.NetworkModeAllowlist)
	cfg.WorktreePath = "/home/dev/wt"
	cfg.PublishPorts = []int{3000}

	out := buildConfig(cfg, vmPaths{}, "/tmp/worktree.ext4")

	assert.True(t, guestboot.ParseGuestNetwork(out.KernelArgs).Valid())
	assert.Equal(t, "/home/dev/wt", guestboot.ParseWorktreeMount(out.KernelArgs).Path)
	assert.Equal(t, []int{3000}, guestboot.ParsePublishPorts(out.KernelArgs))
	assert.True(t, guestboot.ParseOverlayUpper(out.KernelArgs).Valid())
	assert.Contains(t, out.KernelArgs, "console=ttyS0 reboot=k")
}
