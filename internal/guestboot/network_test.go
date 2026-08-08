package guestboot

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGuestNetworkRoundTrip verifies a network descriptor appended by the
// host parses back identically in the guest — the contract that replaces the
// guest hardcoding the addresses netgw.go already owns. Refs: MGIT-68
func TestGuestNetworkRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		net  GuestNetwork
	}{
		{
			name: "libkrun_netstack_link",
			net:  GuestNetwork{IP: "10.0.2.15", PrefixLen: 24, Gateway: "10.0.2.2", DNS: "10.0.2.2"},
		},
		{
			name: "point_to_point_link",
			net:  GuestNetwork{IP: "172.31.4.2", PrefixLen: 30, Gateway: "172.31.4.1", DNS: "172.31.4.1"},
		},
		{
			name: "no_resolver",
			net:  GuestNetwork{IP: "10.0.2.15", PrefixLen: 24, Gateway: "10.0.2.2"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseGuestNetwork(AppendNetworkCmdline("console=ttyS0 reboot=k", tt.net))
			assert.Equal(t, tt.net, got)
			assert.True(t, got.Valid())
			assert.False(t, got.Empty())
		})
	}
}

// TestAppendNetworkCmdline_EmptyBase verifies no leading space when the base
// is blank (the libkrun env channel starts empty).
func TestAppendNetworkCmdline_EmptyBase(t *testing.T) {
	out := AppendNetworkCmdline("", GuestNetwork{IP: "10.0.2.15", PrefixLen: 24, Gateway: "10.0.2.2", DNS: "10.0.2.2"})
	assert.Equal(t, "mgit.net_ip=10.0.2.15 mgit.net_prefix=24 mgit.net_gw=10.0.2.2 mgit.net_dns=10.0.2.2", out)
}

// TestAppendNetworkCmdline_NoDescriptor_AddsNothing verifies a sandbox with
// no network (none mode) leaves the token stream untouched, so the guest can
// tell "no network was configured for me" from "network config failed".
func TestAppendNetworkCmdline_NoDescriptor_AddsNothing(t *testing.T) {
	assert.Equal(t, "console=ttyS0", AppendNetworkCmdline("console=ttyS0", GuestNetwork{}))
}

// TestAppendNetworkCmdline_IncoherentDescriptor_TravelsAndIsRejected pins a
// deliberate choice: a partial descriptor is EMITTED, not silently dropped.
// It can only come from a host-side mistake, and a guest that fails its boot
// naming the bad descriptor is far easier to diagnose than the silently
// network-less guest dropping it would produce — which is precisely the
// failure MGIT-68 shipped. Refs: MGIT-68, MGIT-69
func TestAppendNetworkCmdline_IncoherentDescriptor_TravelsAndIsRejected(t *testing.T) {
	out := AppendNetworkCmdline("console=ttyS0", GuestNetwork{Gateway: "10.0.2.2"})

	assert.Equal(t, "console=ttyS0 mgit.net_gw=10.0.2.2", out, "no empty keys on the wire")
	got := ParseGuestNetwork(out)
	assert.Equal(t, GuestNetwork{Gateway: "10.0.2.2"}, got)
	assert.False(t, got.Valid(), "the guest must reject it rather than half-configure")
}

// TestParseGuestNetwork_IgnoresUnrelatedTokens verifies the network keys are
// extracted from a realistic token stream carrying every other descriptor.
func TestParseGuestNetwork_IgnoresUnrelatedTokens(t *testing.T) {
	tokens := "console=ttyS0 mgit.worktree=/wt mgit.net_ip=10.0.2.15 root=/dev/vda " +
		"mgit.net_prefix=24 mgit.publish_ports=3000 mgit.net_gw=10.0.2.2 mgit.net_dns=10.0.2.2"
	assert.Equal(t, GuestNetwork{IP: "10.0.2.15", PrefixLen: 24, Gateway: "10.0.2.2", DNS: "10.0.2.2"},
		ParseGuestNetwork(tokens))
}

// TestParseGuestNetwork_Absent verifies a token stream with no network keys
// yields an empty descriptor (none mode: no NIC configuration attempted).
func TestParseGuestNetwork_Absent(t *testing.T) {
	got := ParseGuestNetwork("console=ttyS0 mgit.worktree=/wt")
	assert.True(t, got.Empty())
	assert.False(t, got.Valid())
}

// TestGuestNetwork_Valid rejects every partial or malformed descriptor. A
// guest must fail closed on one of these rather than configure a NIC from
// half a descriptor. Refs: MGIT-68
func TestGuestNetwork_Valid(t *testing.T) {
	tests := []struct {
		name string
		net  GuestNetwork
		want bool
	}{
		{"complete", GuestNetwork{IP: "10.0.2.15", PrefixLen: 24, Gateway: "10.0.2.2"}, true},
		{"no_dns_is_fine", GuestNetwork{IP: "10.0.2.15", PrefixLen: 24, Gateway: "10.0.2.2"}, true},
		{"missing_ip", GuestNetwork{PrefixLen: 24, Gateway: "10.0.2.2"}, false},
		{"missing_gateway", GuestNetwork{IP: "10.0.2.15", PrefixLen: 24}, false},
		{"zero_prefix", GuestNetwork{IP: "10.0.2.15", Gateway: "10.0.2.2"}, false},
		{"prefix_too_large", GuestNetwork{IP: "10.0.2.15", PrefixLen: 33, Gateway: "10.0.2.2"}, false},
		{"ip_not_an_address", GuestNetwork{IP: "not-an-ip", PrefixLen: 24, Gateway: "10.0.2.2"}, false},
		{"gateway_not_an_address", GuestNetwork{IP: "10.0.2.15", PrefixLen: 24, Gateway: "nope"}, false},
		{"ipv6_is_not_served", GuestNetwork{IP: "fd00::15", PrefixLen: 64, Gateway: "fd00::2"}, false},
		{"dns_not_an_address", GuestNetwork{IP: "10.0.2.15", PrefixLen: 24, Gateway: "10.0.2.2", DNS: "nope"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.net.Valid())
		})
	}
}

// TestParseGuestNetwork_MalformedPrefix verifies a non-numeric or
// out-of-range prefix does not silently become a usable descriptor.
func TestParseGuestNetwork_MalformedPrefix(t *testing.T) {
	for _, tokens := range []string{
		"mgit.net_ip=10.0.2.15 mgit.net_prefix=abc mgit.net_gw=10.0.2.2",
		"mgit.net_ip=10.0.2.15 mgit.net_prefix=99 mgit.net_gw=10.0.2.2",
		"mgit.net_ip=10.0.2.15 mgit.net_prefix=-1 mgit.net_gw=10.0.2.2",
	} {
		got := ParseGuestNetwork(tokens)
		assert.False(t, got.Valid(), "tokens %q must not yield a valid descriptor", tokens)
		assert.False(t, got.Empty(), "tokens %q carry an address, so this is malformed, not absent", tokens)
	}
}

// TestGuestNetwork_Resolver verifies the resolver defaults to the gateway —
// the gateway IS where the host-side pinning resolver listens (SEC-07), so an
// omitted mgit.net_dns must not leave the guest with no nameserver.
func TestGuestNetwork_Resolver(t *testing.T) {
	assert.Equal(t, "10.0.2.2", GuestNetwork{IP: "10.0.2.15", PrefixLen: 24, Gateway: "10.0.2.2"}.Resolver())
	assert.Equal(t, "10.0.2.3",
		GuestNetwork{IP: "10.0.2.15", PrefixLen: 24, Gateway: "10.0.2.2", DNS: "10.0.2.3"}.Resolver())
}

// TestGuestNetwork_Netmask verifies the dotted-quad netmask the guest's
// SIOCSIFNETMASK ioctl needs is derived from the prefix rather than restated.
func TestGuestNetwork_Netmask(t *testing.T) {
	tests := map[int]string{24: "255.255.255.0", 30: "255.255.255.252", 16: "255.255.0.0", 32: "255.255.255.255"}
	for prefix, want := range tests {
		mask := GuestNetwork{IP: "10.0.2.15", PrefixLen: prefix, Gateway: "10.0.2.2"}.Netmask()
		require.NotNil(t, mask)
		assert.Equal(t, want, mask.String())
	}
	assert.Nil(t, GuestNetwork{IP: "10.0.2.15", PrefixLen: 0, Gateway: "10.0.2.2"}.Netmask(),
		"an invalid descriptor has no netmask")
}

// TestGuestNetworkRoundTrip_ResolverOnly verifies the RESOLVER-ONLY shape: a
// backend whose guest is already addressed by other means (firecracker's
// kernel `ip=` autoconfiguration) needs only to be told its nameserver, and
// must not be handed a second, redundant link configuration. Refs: MGIT-69
func TestGuestNetworkRoundTrip_ResolverOnly(t *testing.T) {
	n := GuestNetwork{DNS: "172.31.4.1"}

	got := ParseGuestNetwork(AppendNetworkCmdline("console=ttyS0 ip=172.31.4.2::172.31.4.1:255.255.255.252::eth0:off", n))

	assert.Equal(t, n, got)
	assert.True(t, got.Valid(), "a resolver-only descriptor is coherent")
	assert.False(t, got.Empty())
	assert.False(t, got.ConfiguresLink(), "resolver-only must not claim link configuration")
	assert.Equal(t, "172.31.4.1", got.Resolver())
}

// TestGuestNetwork_ConfiguresLink separates the two shapes the guest must
// tell apart: a full link descriptor it applies with ioctls, and a
// resolver-only one where the address is already the kernel's business.
func TestGuestNetwork_ConfiguresLink(t *testing.T) {
	assert.True(t, GuestNetwork{IP: "10.0.2.15", PrefixLen: 24, Gateway: "10.0.2.2"}.ConfiguresLink())
	assert.False(t, GuestNetwork{DNS: "10.0.2.2"}.ConfiguresLink())
	assert.False(t, GuestNetwork{}.ConfiguresLink())
	// A descriptor carrying ANY link field claims link configuration, so a
	// partial one is rejected as incomplete rather than silently downgraded
	// to resolver-only.
	assert.True(t, GuestNetwork{Gateway: "10.0.2.2", DNS: "10.0.2.2"}.ConfiguresLink())
	assert.False(t, GuestNetwork{Gateway: "10.0.2.2", DNS: "10.0.2.2"}.Valid())
}

// TestAppendNetworkCmdline_ResolverOnly verifies only the resolver token is
// emitted — no empty address/prefix keys the guest would have to special-case.
func TestAppendNetworkCmdline_ResolverOnly(t *testing.T) {
	assert.Equal(t, "mgit.net_dns=172.31.4.1", AppendNetworkCmdline("", GuestNetwork{DNS: "172.31.4.1"}))
}

// TestGuestNetwork_Valid_ResolverOnly rejects a malformed resolver-only
// descriptor rather than leaving the guest pointed at nonsense.
func TestGuestNetwork_Valid_ResolverOnly(t *testing.T) {
	assert.True(t, GuestNetwork{DNS: "10.0.2.2"}.Valid())
	assert.False(t, GuestNetwork{DNS: "not-an-ip"}.Valid())
	assert.False(t, GuestNetwork{DNS: "fd00::2"}.Valid(), "the gateways serve IPv4 only")
}
