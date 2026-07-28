// Command netguest is the GUEST workload for the libkrun real-VM e2e. It runs
// as PID 1 inside a real microVM whose virtio-net device is backed by mgit's
// netstack egress gateway, configures its NIC, and reports what it can and
// cannot reach. The host asserts on the GUEST-RESULT lines in the VM console.
//
// It lives under testdata/ so the normal build never compiles it; the test
// cross-compiles it for linux/<arch> on demand. Refs: MGIT-61.10, ADR-010
package main

import (
	"fmt"
	"net"
	"os"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// The virtual network mgit's gateway serves (netgw.go: guestIP/gatewayIP).
const (
	guestAddr = "10.0.2.15"
	netmask   = "255.255.255.0"
	gateway   = "10.0.2.2"
	// guestNIC is selected by NAME on purpose: the guest also enumerates a
	// dummy0 interface, so "the first non-lo interface" picks the wrong one
	// and every dial fails "no route to host" (ADR-010).
	guestNIC = "eth0"
	// offAllowlistDest is a destination no test policy names. It is a PUBLIC
	// address on purpose: a reserved/documentation range would be refused by
	// the unconditional IP denials (SEC-04/T9) BEFORE the allowlist is ever
	// consulted, so the test would pass without proving default-deny. Nothing
	// ever dials it — the point is that the flow is refused host-side.
	offAllowlistDest = "93.184.216.34:443"
)

type ifreq struct {
	name [16]byte
	data [24]byte
}

func sockaddrIn(ip string) [24]byte {
	var d [24]byte
	*(*uint16)(unsafe.Pointer(&d[0])) = unix.AF_INET
	copy(d[4:8], net.ParseIP(ip).To4())
	return d
}

func ioctlIf(fd int, req uint, name string, data [24]byte) error {
	var r ifreq
	copy(r.name[:], name)
	r.data = data
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(req), uintptr(unsafe.Pointer(&r))); errno != 0 {
		return errno
	}
	return nil
}

// rtentry mirrors struct rtentry for SIOCADDRT (the default route).
type rtentry struct {
	pad0    uint64
	dst     [16]byte
	gateway [16]byte
	genmask [16]byte
	flags   uint16
	_       [6]byte
	pad3    uint64
	pad4    uint64
	pad5    uint64
	tos     uint8
	class   uint8
	_       [3]int16
	metric  int16
	dev     uintptr
	mtu     uint64
	window  uint64
	irtt    uint16
	_       [6]byte
}

func sockaddr16(ip string) [16]byte {
	var d [16]byte
	*(*uint16)(unsafe.Pointer(&d[0])) = unix.AF_INET
	if ip != "" {
		copy(d[4:8], net.ParseIP(ip).To4())
	}
	return d
}

// configureNIC brings the interface up with a static address and default
// route. Production wants DHCP or an ip= cmdline; ioctls keep the guest
// dependency-free while the addressing decision is still open (MGIT-61.10).
func configureNIC(iface string) error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
	if err != nil {
		return fmt.Errorf("socket: %w", err)
	}
	defer func() { _ = unix.Close(fd) }()

	if err := ioctlIf(fd, unix.SIOCSIFADDR, iface, sockaddrIn(guestAddr)); err != nil {
		return fmt.Errorf("set addr: %w", err)
	}
	if err := ioctlIf(fd, unix.SIOCSIFNETMASK, iface, sockaddrIn(netmask)); err != nil {
		return fmt.Errorf("set netmask: %w", err)
	}
	var flags [24]byte
	*(*uint16)(unsafe.Pointer(&flags[0])) = unix.IFF_UP | unix.IFF_RUNNING
	if err := ioctlIf(fd, unix.SIOCSIFFLAGS, iface, flags); err != nil {
		return fmt.Errorf("set up: %w", err)
	}

	rt := rtentry{
		dst:     sockaddr16(""),
		gateway: sockaddr16(gateway),
		genmask: sockaddr16(""),
		flags:   0x0001 | 0x0002, // RTF_UP | RTF_GATEWAY
	}
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd),
		unix.SIOCADDRT, uintptr(unsafe.Pointer(&rt))); errno != 0 {
		return fmt.Errorf("add default route: %w", errno)
	}
	return nil
}

// probe reports whether one destination is reachable from inside the guest.
func probe(label, addr string) {
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		fmt.Printf("GUEST-RESULT %s = DENIED (%v)\n", label, err)
		return
	}
	defer func() { _ = c.Close() }()
	fmt.Printf("GUEST-RESULT %s = ALLOWED\n", label)
}

func main() {
	fmt.Println("GUEST: booted inside a real libkrun microVM")

	ifaces, _ := net.Interfaces()
	names := make([]string, 0, len(ifaces))
	for _, i := range ifaces {
		names = append(names, i.Name)
	}
	fmt.Printf("GUEST: interfaces=%v\n", names)

	if err := configureNIC(guestNIC); err != nil {
		fmt.Printf("GUEST: configure %s failed: %v\n", guestNIC, err)
		os.Exit(1)
	}
	fmt.Printf("GUEST: %s configured %s/24 gw %s\n", guestNIC, guestAddr, gateway)

	// The destination is BAKED IN rather than taken from argv: the production
	// spec's ExecArgs are mgit-guest's flags, and libkrun additionally
	// PREPENDS the executable to argv (ADR-010), so argv is not a usable
	// channel for test parameters here.
	probe("OFF_ALLOWLIST", offAllowlistDest)
	fmt.Println("GUEST: done")
}
