//go:build linux

// buildConfig is a pure translation of VMConfig to a Firecracker config, so
// these tests need no /dev/kvm or firecracker binary — they run on any Linux
// (CI included). The real boot + egress round-trip is the KVM-gated e2e
// suite. Refs: FR-17.7, FR-17.8, MGIT-11.7.1, MGIT-11.7.2
package firecracker

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/backend/microvm"
	"github.com/hyper-swe/mgit/internal/sandboxd/egress"
)

func nicBaseConfig() microvm.VMConfig {
	return microvm.VMConfig{
		SandboxID: "01JABCDEF0123456789KLMNOPQ",
		CPUs:      1, MemoryMB: 128,
		KernelPath: "/k", RootfsPath: "/r", RootfsReadOnly: true,
		Cmdline: "console=hvc0", VsockEnabled: true,
	}
}

// TestBuildConfig_NoneAttachesNoNIC verifies none mode produces no network
// interface (the guest has no NIC and cannot egress). Refs: FR-17.7, MGIT-11.7.1
func TestBuildConfig_NoneAttachesNoNIC(t *testing.T) {
	cfg := nicBaseConfig()
	cfg.AttachNIC, cfg.NetworkMode = false, model.NetworkModeNone
	out := buildConfig(cfg, sandboxPaths(t.TempDir()), "")
	assert.Empty(t, out.NetworkInterfaces, "none mode: no NIC")
}

// TestBuildConfig_AttachesStaticNIC verifies allowlist/open modes attach a
// static NIC on the sandbox's /30 with the host tap as gateway and the host
// resolver (gateway) as the sole nameserver. Refs: FR-17.7, FR-17.8, MGIT-11.7.1
func TestBuildConfig_AttachesStaticNIC(t *testing.T) {
	for _, mode := range []string{model.NetworkModeAllowlist, model.NetworkModeOpen} {
		t.Run(mode, func(t *testing.T) {
			cfg := nicBaseConfig()
			cfg.AttachNIC, cfg.NetworkMode = true, mode
			out := buildConfig(cfg, sandboxPaths(t.TempDir()), "")

			require.Len(t, out.NetworkInterfaces, 1)
			sc := out.NetworkInterfaces[0].StaticConfiguration
			require.NotNil(t, sc, "a static NIC config")
			assert.Equal(t, egress.TapName(cfg.SandboxID), sc.HostDevName, "NIC bound to the per-sandbox tap")
			assert.Equal(t, guestMAC(cfg.SandboxID), sc.MacAddress)

			require.NotNil(t, sc.IPConfiguration)
			gw, _, guestNet := subnetFor(cfg.SandboxID)
			assert.Equal(t, gw.String(), sc.IPConfiguration.Gateway.String(), "host tap is the gateway")
			assert.Equal(t, guestNet.String(), sc.IPConfiguration.IPAddr.String(), "guest IP on its /30")
			assert.Equal(t, []string{gw.String()}, sc.IPConfiguration.Nameservers,
				"the only nameserver is the host resolver on the gateway (host-side DNS, SEC-07)")
		})
	}
}
