//go:build linux

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/guestboot"
)

// withResolvPath points the guest's resolver file at a temp path so these
// tests never touch the host's /etc/resolv.conf.
func withResolvPath(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "etc", "resolv.conf")
	orig := guestResolvPath
	guestResolvPath = p
	t.Cleanup(func() { guestResolvPath = orig })
	return p
}

// TestConfigureGuestNetwork_NoDescriptor_NoOp verifies a none-mode sandbox
// (no network tokens on either transport) boots without attempting any NIC
// configuration — and without needing privilege. Refs: MGIT-68, FR-17.7
func TestConfigureGuestNetwork_NoDescriptor_NoOp(t *testing.T) {
	withCmdline(t, "console=ttyS0 root=/dev/vda")
	t.Setenv(guestboot.EnvBootTokens, "")
	resolv := withResolvPath(t)

	require.NoError(t, configureGuestNetwork(quietLogger()))

	_, err := os.Stat(resolv)
	assert.True(t, os.IsNotExist(err), "a network-less sandbox writes no resolver config")
}

// TestConfigureGuestNetwork_PartialDescriptor_FailsClosed verifies a
// half-specified descriptor aborts the boot with a reason rather than leaving
// the guest with a partly configured NIC. Refs: MGIT-68
func TestConfigureGuestNetwork_PartialDescriptor_FailsClosed(t *testing.T) {
	withCmdline(t, "console=ttyS0")
	// Address and prefix, no gateway: a guest with no default route.
	t.Setenv(guestboot.EnvBootTokens, "mgit.net_ip=10.0.2.15 mgit.net_prefix=24")
	withResolvPath(t)

	err := configureGuestNetwork(quietLogger())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "incomplete guest network descriptor")
}

// TestConfigureGuestNetwork_ReadsTheEnvChannel verifies the descriptor is
// taken from the ENV boot-token channel — the only one libkrun has, since it
// boots libkrunfw's own kernel and mgit composes no command line for it. A
// guest that only read /proc/cmdline would stay unaddressed on exactly the
// backend MGIT-68 was reported against. Refs: MGIT-68, ADR-010
func TestConfigureGuestNetwork_ReadsTheEnvChannel(t *testing.T) {
	withCmdline(t, "console=ttyS0 root=/dev/vda")
	t.Setenv(guestboot.EnvBootTokens,
		"mgit.net_ip=10.0.2.15 mgit.net_prefix=24 mgit.net_gw=10.0.2.2 mgit.net_dns=10.0.2.2")

	got := guestboot.ParseGuestNetwork(bootTokens())

	assert.Equal(t, guestboot.GuestNetwork{
		IP: "10.0.2.15", PrefixLen: 24, Gateway: "10.0.2.2", DNS: "10.0.2.2",
	}, got)
	assert.True(t, got.Valid())
}
