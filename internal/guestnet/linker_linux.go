//go:build linux

package guestnet

import (
	"fmt"
	"net"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/hyper-swe/mgit/internal/guestboot"
)

// NewLinker returns the Linux link configurator: the SIOCSIF* / SIOCADDRT
// ioctl sequence that gives the guest NIC its address and default route.
//
// Why ioctls and not `ip`: the guest image is minimal and mgit-guest is PID 1
// with no PATH and no shell — there is no iproute2 to exec, and shelling out
// of PID 1 to configure the network would be a new failure mode for no gain.
// Why not netlink: RTM_NEWADDR/RTM_NEWROUTE would be several hundred lines of
// hand-rolled message marshaling for the same three operations. Refs: MGIT-68
func NewLinker() Linker { return unixLinker{} }

// unixLinker applies the address, netmask, up-flag and default route.
type unixLinker struct{}

// Configure brings iface up on n's address and installs n's default route.
// The sequence is address, netmask, UP, route — a route through a gateway
// that is not yet on-link is rejected, so the address must land first.
// Refs: MGIT-68, FR-17.7
func (unixLinker) Configure(iface string, n guestboot.GuestNetwork) error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
	if err != nil {
		return fmt.Errorf("open configuration socket: %w", err)
	}
	defer func() { _ = unix.Close(fd) }()

	if err := setInet4(fd, unix.SIOCSIFADDR, iface, n.IP); err != nil {
		return fmt.Errorf("set address %s: %w", n.IP, err)
	}
	mask := n.Netmask()
	if err := setInet4(fd, unix.SIOCSIFNETMASK, iface, mask.String()); err != nil {
		return fmt.Errorf("set netmask %s: %w", mask, err)
	}
	if err := bringUp(fd, iface); err != nil {
		return err
	}
	if err := addDefaultRoute(fd, n.Gateway); err != nil {
		return fmt.Errorf("add default route via %s: %w", n.Gateway, err)
	}
	return nil
}

// setInet4 issues one ifreq ioctl carrying an IPv4 address.
func setInet4(fd int, req uint, iface, addr string) error {
	ifr, err := unix.NewIfreq(iface)
	if err != nil {
		return fmt.Errorf("ifreq for %s: %w", iface, err)
	}
	ip := net.ParseIP(addr)
	if ip == nil || ip.To4() == nil {
		return fmt.Errorf("%q is not an IPv4 address", addr)
	}
	if err := ifr.SetInet4Addr(ip.To4()); err != nil {
		return err
	}
	return unix.IoctlIfreq(fd, req, ifr)
}

// bringUp sets IFF_UP|IFF_RUNNING while PRESERVING the interface's existing
// flags: overwriting them would clear whatever the kernel already set for the
// device (broadcast, multicast) rather than adding to it.
func bringUp(fd int, iface string) error {
	ifr, err := unix.NewIfreq(iface)
	if err != nil {
		return fmt.Errorf("ifreq for %s: %w", iface, err)
	}
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFFLAGS, ifr); err != nil {
		return fmt.Errorf("read %s flags: %w", iface, err)
	}
	ifr.SetUint16(ifr.Uint16() | unix.IFF_UP | unix.IFF_RUNNING)
	if err := unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, ifr); err != nil {
		return fmt.Errorf("bring %s up: %w", iface, err)
	}
	return nil
}

// Route flags from linux/route.h: the route is usable, and it is via a
// gateway rather than directly attached.
const (
	rtfUp      = 0x0001
	rtfGateway = 0x0002
)

// rtentry mirrors the kernel's struct rtentry (linux/route.h) for SIOCADDRT,
// field for field including its padding, on 64-bit Linux — the only shape
// mgit ships a guest for. Only rt_dst, rt_gateway, rt_genmask and rt_flags
// are set; every other field must be zero (a NULL rt_dev means "let the
// kernel pick the device from the gateway's on-link address").
type rtentry struct {
	pad1    uint64
	dst     [16]byte // struct sockaddr
	gateway [16]byte // struct sockaddr
	genmask [16]byte // struct sockaddr
	flags   uint16
	pad2    int16
	_       [4]byte // alignment to the next unsigned long
	pad3    uint64
	tos     uint8
	class   uint8
	pad4    [3]int16
	metric  int16
	dev     uintptr // char __user *rt_dev
	mtu     uint64
	window  uint64
	irtt    uint16
	_       [6]byte // tail padding
}

// addDefaultRoute installs 0.0.0.0/0 via gateway.
func addDefaultRoute(fd int, gateway string) error {
	gw := net.ParseIP(gateway)
	if gw == nil || gw.To4() == nil {
		return fmt.Errorf("%q is not an IPv4 address", gateway)
	}
	rt := rtentry{
		dst:     sockaddrInet4(nil), // 0.0.0.0
		gateway: sockaddrInet4(gw.To4()),
		genmask: sockaddrInet4(nil), // /0
		flags:   rtfUp | rtfGateway,
	}
	//nolint:gosec // G103: SIOCADDRT takes a struct pointer; there is no ifreq-style wrapper for it
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), unix.SIOCADDRT,
		uintptr(unsafe.Pointer(&rt))); errno != 0 && errno != unix.EEXIST {
		// EEXIST is tolerated for boot idempotence, the same way the guest's
		// mounts tolerate EBUSY: a backend whose kernel already installed this
		// route from an `ip=` boot parameter (firecracker) must not have its
		// boot failed by mgit-guest asking for the route it already has.
		return errno
	}
	return nil
}

// sockaddrInet4 renders an IPv4 address as a struct sockaddr (family, port,
// address, padding). A nil address yields 0.0.0.0, which is what both the
// default route's destination and its genmask are.
func sockaddrInet4(ip net.IP) [16]byte {
	var sa [16]byte
	// struct sockaddr_in { sa_family_t sin_family; in_port_t sin_port;
	//                      struct in_addr sin_addr; char sin_zero[8]; }
	sa[0] = byte(unix.AF_INET)
	sa[1] = byte(unix.AF_INET >> 8)
	copy(sa[4:8], ip)
	return sa
}
