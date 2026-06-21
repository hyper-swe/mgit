package egress

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/netip"

	"github.com/hyper-swe/mgit/internal/model"
)

// tapPrefix namespaces mgit's host tap devices. The full name is
// prefix+11 hex chars = 14 bytes, within Linux IFNAMSIZ (15). Refs: FR-17.7
const tapPrefix = "mgt"

// TapName derives a deterministic, collision-resistant host tap interface
// name from a sandbox ID, bounded to the 15-byte interface-name limit. The
// linux backend creates this device; teardown deletes it (no residue,
// FR-17.19). Refs: FR-17.7, FR-17.19
func TapName(sandboxID string) string {
	sum := sha256.Sum256([]byte(sandboxID))
	return tapPrefix + hex.EncodeToString(sum[:])[:11]
}

// TapPlan describes the host-side network plumbing for one sandbox's tap, in
// a backend-neutral form. SetupCommands renders the privileged host commands
// (`ip`, `iptables`) that realize the policy at the IP/flow layer; the linux
// backend execs them. The security invariant lives here: in allowlist mode
// the guest gets NO direct route — only the host proxy and the host DNS
// resolver are reachable, everything else is dropped, and there is NO NAT.
// Refs: SEC-04, FR-17.7, FR-17.8
type TapPlan struct {
	Mode      string     // model.NetworkMode*
	TapDev    string     // host tap interface name (TapName)
	GuestIP   netip.Addr // guest side of the point-to-point link
	GatewayIP netip.Addr // host side of the tap; the proxy + resolver listen here
	ProxyPort int        // host egress proxy port on GatewayIP (allowlist mode)
	DNSPort   int        // host resolver port on GatewayIP (allowlist mode)
	ExtIface  string     // host external interface to NAT through (open mode)
}

// SetupCommands renders the ordered host commands that bring up the tap and
// install the per-mode policy. none mode returns no commands (no NIC).
// Refs: SEC-04, FR-17.7, FR-17.8
func (p TapPlan) SetupCommands() ([][]string, error) {
	if p.Mode == model.NetworkModeNone {
		return nil, nil // no NIC, nothing to plumb
	}
	if err := p.validate(); err != nil {
		return nil, err
	}
	cmds := p.linkUpCommands()
	switch p.Mode {
	case model.NetworkModeAllowlist:
		return append(cmds, p.allowlistRules()...), nil
	case model.NetworkModeOpen:
		return append(cmds, p.openRules()...), nil
	default:
		return nil, fmt.Errorf("tap plan: unknown network mode %q", p.Mode)
	}
}

// linkUpCommands creates the tap, assigns the host gateway IP on a /30
// point-to-point link, and brings it up.
func (p TapPlan) linkUpCommands() [][]string {
	gw := p.GatewayIP.String() + "/30"
	return [][]string{
		{"ip", "tuntap", "add", "dev", p.TapDev, "mode", "tap"},
		{"ip", "addr", "add", gw, "dev", p.TapDev},
		{"ip", "link", "set", p.TapDev, "up"},
	}
}

// allowlistRules give the guest no direct route: it may reach only the host
// proxy and the host resolver on the gateway; all other guest INPUT is
// dropped and no traffic is forwarded out (no NAT). Order matters — the
// ACCEPTs precede the DROPs. Refs: SEC-04, FR-17.8
func (p TapPlan) allowlistRules() [][]string {
	gw := p.GatewayIP.String()
	proxy := fmt.Sprintf("%d", p.ProxyPort)
	dns := fmt.Sprintf("%d", p.DNSPort)
	return [][]string{
		{"iptables", "-A", "INPUT", "-i", p.TapDev, "-p", "tcp", "-d", gw, "--dport", proxy, "-j", "ACCEPT"},
		{"iptables", "-A", "INPUT", "-i", p.TapDev, "-p", "udp", "-d", gw, "--dport", dns, "-j", "ACCEPT"},
		{"iptables", "-A", "INPUT", "-i", p.TapDev, "-p", "tcp", "-d", gw, "--dport", dns, "-j", "ACCEPT"},
		// default-deny everything else the guest sends to the host...
		{"iptables", "-A", "INPUT", "-i", p.TapDev, "-j", "DROP"},
		// ...and forward nothing out: the guest has no route to the internet.
		{"iptables", "-A", "FORWARD", "-i", p.TapDev, "-j", "DROP"},
	}
}

// openRules NAT the guest to the host network (full egress) — the explicitly
// risky posture (T3/T9 disabled). Refs: FR-17.7
func (p TapPlan) openRules() [][]string {
	return [][]string{
		{"sysctl", "-w", "net.ipv4.ip_forward=1"},
		{"iptables", "-A", "FORWARD", "-i", p.TapDev, "-o", p.ExtIface, "-j", "ACCEPT"},
		{"iptables", "-A", "FORWARD", "-i", p.ExtIface, "-o", p.TapDev, "-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT"},
		{"iptables", "-t", "nat", "-A", "POSTROUTING", "-s", p.GuestIP.String(), "-o", p.ExtIface, "-j", "MASQUERADE"},
	}
}

// TeardownCommands delete the tap, which also drops its addresses and the
// interface-scoped filter rules, leaving no host residue. Refs: FR-17.19
func (p TapPlan) TeardownCommands() [][]string {
	if p.Mode == model.NetworkModeNone || p.TapDev == "" {
		return nil
	}
	cmds := [][]string{}
	if p.Mode == model.NetworkModeOpen && p.ExtIface != "" && p.GuestIP.IsValid() {
		// The NAT rule is in the nat table and not interface-scoped on the
		// tap, so it must be explicitly removed.
		cmds = append(cmds, []string{"iptables", "-t", "nat", "-D", "POSTROUTING",
			"-s", p.GuestIP.String(), "-o", p.ExtIface, "-j", "MASQUERADE"})
	}
	return append(cmds, []string{"ip", "link", "del", p.TapDev})
}

// validate rejects an incomplete plan so a misconfigured tap fails closed
// rather than launching a guest with a half-applied policy. Refs: SEC-04
func (p TapPlan) validate() error {
	switch {
	case p.TapDev == "":
		return fmt.Errorf("tap plan: tap device must not be empty")
	case !p.GuestIP.IsValid() || !p.GatewayIP.IsValid():
		return fmt.Errorf("tap plan: guest and gateway IPs must be valid")
	}
	switch p.Mode {
	case model.NetworkModeAllowlist:
		if p.ProxyPort < 1 || p.ProxyPort > 65535 {
			return fmt.Errorf("tap plan: allowlist mode needs a valid proxy port, got %d", p.ProxyPort)
		}
		if p.DNSPort < 1 || p.DNSPort > 65535 {
			return fmt.Errorf("tap plan: allowlist mode needs a valid dns port, got %d", p.DNSPort)
		}
	case model.NetworkModeOpen:
		if p.ExtIface == "" {
			return fmt.Errorf("tap plan: open mode needs an external interface to NAT through")
		}
	}
	return nil
}
