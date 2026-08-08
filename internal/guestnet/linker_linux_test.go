//go:build linux

package guestnet

import (
	"testing"
	"unsafe"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// TestRtentry_MatchesKernelLayout pins the SIOCADDRT struct's size and the
// offsets of the four fields the kernel reads. A silently mis-padded struct
// would place the gateway somewhere the kernel does not look and yield a
// route that exists but goes nowhere — which is very hard to tell apart from
// a policy denial, the whole class of failure MGIT-68 belongs to.
// Refs: MGIT-68
func TestRtentry_MatchesKernelLayout(t *testing.T) {
	var rt rtentry
	// 64-bit linux/route.h: rt_pad1(8) + 3 sockaddrs(48) + flags/pad(8) +
	// rt_pad3(8) + tos/class/pad4(8) + metric+pad(8) + rt_dev(8) + mtu(8) +
	// window(8) + irtt+pad(8).
	assert.Equal(t, uintptr(120), unsafe.Sizeof(rt))
	assert.Equal(t, uintptr(8), unsafe.Offsetof(rt.dst))
	assert.Equal(t, uintptr(24), unsafe.Offsetof(rt.gateway))
	assert.Equal(t, uintptr(40), unsafe.Offsetof(rt.genmask))
	assert.Equal(t, uintptr(56), unsafe.Offsetof(rt.flags))
	assert.Equal(t, uintptr(88), unsafe.Offsetof(rt.dev), "rt_dev must be NULL-able where the kernel reads it")
}

// TestSockaddrInet4 verifies the sockaddr rendering the route ioctl carries:
// AF_INET in the family field and the address at offset 4.
func TestSockaddrInet4(t *testing.T) {
	sa := sockaddrInet4([]byte{10, 0, 2, 2})
	assert.Equal(t, byte(unix.AF_INET), sa[0])
	assert.Equal(t, []byte{10, 0, 2, 2}, sa[4:8])

	zero := sockaddrInet4(nil)
	assert.Equal(t, []byte{0, 0, 0, 0}, zero[4:8], "a nil address is 0.0.0.0 (default route/genmask)")
}

// TestNewLinker_IsWired verifies the Linux build supplies a real
// configurator, since Apply fails closed without one.
func TestNewLinker_IsWired(t *testing.T) {
	require.NotNil(t, NewLinker())
}

// TestUnixLinker_RejectsMalformedAddresses verifies the address guards fire
// before any ioctl, so a bad descriptor cannot reach the kernel. It needs no
// privilege: the failure happens while building the request.
func TestUnixLinker_RejectsMalformedAddresses(t *testing.T) {
	assert.Error(t, setInet4(-1, unix.SIOCSIFADDR, NIC, "not-an-ip"))
	assert.Error(t, addDefaultRoute(-1, "not-an-ip"))
}
