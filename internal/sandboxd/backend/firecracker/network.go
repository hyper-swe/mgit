package firecracker

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"strings"

	"github.com/hyper-swe/mgit/internal/sandboxd/egress"
)

// rtfUp is the RTF_UP flag bit in /proc/net/route's hex Flags column: a route
// is only usable when it is up. Refs: FR-17.7
const rtfUp = 0x0001

// parseDefaultRouteIface reads a Linux /proc/net/route table and returns the
// interface name of the preferred (lowest-metric, up) default route — the
// Destination 0.0.0.0 row. Open mode NATs the guest out through this
// interface; if there is no default route the host has no upstream and open
// mode fails closed. Parsing host-owned /proc here keeps the detection pure
// and testable (no exec). Refs: FR-17.7
func parseDefaultRouteIface(r io.Reader) (string, error) {
	sc := bufio.NewScanner(r)
	best, bestMetric := "", int64(-1)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		// Columns: Iface Destination Gateway Flags RefCnt Use Metric Mask ...
		if len(fields) < 7 || fields[1] == "Destination" {
			continue // header or short line
		}
		if fields[1] != "00000000" { // not the default route (0.0.0.0)
			continue
		}
		flags, err := strconv.ParseUint(fields[3], 16, 32)
		if err != nil || flags&rtfUp == 0 {
			continue // unparseable flags, or route not up
		}
		metric, err := strconv.ParseInt(fields[6], 10, 64)
		if err != nil {
			continue
		}
		if bestMetric < 0 || metric < bestMetric {
			best, bestMetric = fields[0], metric
		}
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("read route table: %w", err)
	}
	if best == "" {
		return "", fmt.Errorf("no default route found")
	}
	return best, nil
}

// Host gateway ports the egress runtime listens on per sandbox, and the
// ports the allowlist tap firewall steers the guest to. Fixed conventions so
// the backend's firewall plan and the daemon's egress.Runner agree without
// extra plumbing. Refs: SEC-04, FR-17.8
const (
	hostProxyPort = 1080
	hostDNSPort   = 53
)

// NetRunner execs one privileged host network command (ip/iptables).
// Injected so the tap orchestration is testable without root and the real
// exec is confined to the linux build. Refs: FR-17.7
type NetRunner interface {
	Run(ctx context.Context, name string, args ...string) error
}

// applyTapPlan execs a tap plan's setup commands in order, stopping at the
// first failure so a half-applied policy never fronts a booting guest
// (fail closed). none mode yields no commands. Refs: SEC-04, FR-17.7
func applyTapPlan(ctx context.Context, runner NetRunner, plan egress.TapPlan) error {
	cmds, err := plan.SetupCommands()
	if err != nil {
		return fmt.Errorf("tap setup plan: %w", err)
	}
	for _, c := range cmds {
		if err := runner.Run(ctx, c[0], c[1:]...); err != nil {
			return fmt.Errorf("tap setup %v: %w", c, err)
		}
	}
	return nil
}

// removeTapPlan execs a tap plan's teardown commands best-effort: it keeps
// going past individual failures so a partial teardown still removes as much
// host state as possible (no residue, FR-17.19). Refs: FR-17.19
func removeTapPlan(ctx context.Context, runner NetRunner, plan egress.TapPlan) {
	for _, c := range plan.TeardownCommands() {
		_ = runner.Run(ctx, c[0], c[1:]...)
	}
}

// sandboxNetBase is the host-only supernet from which each sandbox gets its
// own /30 point-to-point link to the host tap. It is RFC1918 space — never a
// guest egress target (the egress proxy denies RFC1918); it exists solely as
// the L2 link between the guest NIC and the host gateway. Refs: FR-17.7
var sandboxNetBase = netip.MustParsePrefix("172.31.0.0/16")

// subnetCount is the number of /30 blocks in the /16 supernet (16384).
const subnetCount = 1 << 14

// subnetFor derives a sandbox's deterministic /30 point-to-point link: the
// host gateway (.1) and the guest (.2). Derivation is by hash of the sandbox
// ID, so a sandbox always maps to the same link without shared allocator
// state. The /16 yields 16384 links; collisions across simultaneously-live
// sandboxes are astronomically unlikely at the host's concurrency ceiling
// (FR-17.26). Refs: FR-17.7
func subnetFor(sandboxID string) (gateway, guest netip.Addr, guestNet net.IPNet) {
	sum := sha256.Sum256([]byte(sandboxID))
	block := binary.BigEndian.Uint32(sum[:4]) % subnetCount
	base := ipToU32(sandboxNetBase.Addr()) + block*4
	gateway = u32ToIP(base + 1)
	guest = u32ToIP(base + 2)
	guestNet = net.IPNet{IP: guest.AsSlice(), Mask: net.CIDRMask(30, 32)}
	return gateway, guest, guestNet
}

// GatewayFor reports the host tap gateway IP for a sandbox — the address
// the egress proxy and DNS server bind, and the guest's default gateway. It
// is derived deterministically from the sandbox ID (the same /30 the backend
// configures), so the daemon's egress controller can resolve it without the
// backend reporting it. Refs: FR-17.7, FR-17.8
func GatewayFor(sandboxID string) netip.Addr {
	gateway, _, _ := subnetFor(sandboxID)
	return gateway
}

// guestMAC derives a stable, locally-administered (02:..) unicast MAC for a
// sandbox's guest NIC. Refs: FR-17.7
func guestMAC(sandboxID string) string {
	sum := sha256.Sum256([]byte(sandboxID))
	// 0x02 = locally administered, unicast (low two bits of the first octet).
	return fmt.Sprintf("02:%02x:%02x:%02x:%02x:%02x", sum[0], sum[1], sum[2], sum[3], sum[4])
}

// tapPlanFor builds the shared egress TapPlan for a sandbox's host tap,
// single-sourcing the per-mode firewall invariants from the egress package
// and the fixed gateway ports the egress.Runner binds. extIface is the host
// external interface to NAT through in open mode (empty otherwise).
// Refs: SEC-04, FR-17.7, FR-17.8
func tapPlanFor(sandboxID, mode, extIface string) egress.TapPlan {
	gateway, guest, _ := subnetFor(sandboxID)
	return egress.TapPlan{
		Mode:      mode,
		TapDev:    egress.TapName(sandboxID),
		GuestIP:   guest,
		GatewayIP: gateway,
		ProxyPort: hostProxyPort,
		DNSPort:   hostDNSPort,
		ExtIface:  extIface,
	}
}

// ipToU32 converts an IPv4 netip.Addr to its uint32 value.
func ipToU32(a netip.Addr) uint32 {
	b := a.As4()
	return binary.BigEndian.Uint32(b[:])
}

// u32ToIP converts a uint32 to an IPv4 netip.Addr.
func u32ToIP(v uint32) netip.Addr {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	return netip.AddrFrom4(b)
}
