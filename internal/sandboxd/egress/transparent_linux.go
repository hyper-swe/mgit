//go:build linux

package egress

import (
	"fmt"
	"net"
	"net/netip"

	"golang.org/x/sys/unix"
)

// OriginalDst recovers a redirected connection's ORIGINAL destination from
// conntrack via getsockopt(SO_ORIGINAL_DST).
//
// The kernel's REDIRECT target rewrote the destination to this listener, so
// the socket's own LocalAddr is the gateway, not where the guest was going.
// conntrack remembers the pre-NAT tuple and this is how it is read back. The
// value is HOST-observed — recovered from the kernel, never asserted by the
// guest — which is what makes it a sound basis for a policy decision (SEC-05).
// Refs: MGIT-69, SEC-04, SEC-05
func OriginalDst(conn net.Conn) (netip.AddrPort, error) {
	tcpConn, ok := conn.(*net.TCPConn)
	if !ok {
		return netip.AddrPort{}, fmt.Errorf("original destination: not a TCP connection (%T)", conn)
	}
	raw, err := tcpConn.SyscallConn()
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("original destination: raw conn: %w", err)
	}

	var sa unix.RawSockaddrInet4
	var opErr error
	if err := raw.Control(func(fd uintptr) {
		// SO_ORIGINAL_DST returns a struct sockaddr_in. x/sys exposes no typed
		// getter for it, so the IPv6Mreq accessor is used purely as a
		// correctly-sized byte carrier — the long-standing idiom for this
		// option — and the bytes are decoded explicitly below.
		mreq, gerr := unix.GetsockoptIPv6Mreq(int(fd), unix.IPPROTO_IP, unix.SO_ORIGINAL_DST)
		if gerr != nil {
			opErr = gerr
			return
		}
		// struct sockaddr_in { u16 sin_family; u16 sin_port (network order);
		//                      u32 sin_addr; ... }
		sa.Family = uint16(mreq.Multiaddr[1])<<8 | uint16(mreq.Multiaddr[0])
		sa.Port = uint16(mreq.Multiaddr[2])<<8 | uint16(mreq.Multiaddr[3])
		copy(sa.Addr[:], mreq.Multiaddr[4:8])
	}); err != nil {
		return netip.AddrPort{}, fmt.Errorf("original destination: control: %w", err)
	}
	if opErr != nil {
		return netip.AddrPort{}, fmt.Errorf("original destination: getsockopt SO_ORIGINAL_DST: %w", opErr)
	}
	if sa.Family != unix.AF_INET {
		return netip.AddrPort{}, fmt.Errorf("original destination: unexpected family %d (IPv4 only)", sa.Family)
	}
	return netip.AddrPortFrom(netip.AddrFrom4(sa.Addr), sa.Port), nil
}
