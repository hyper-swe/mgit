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
	// TransparentPort is where the guest's ordinary TCP is REDIRECTed so the
	// transparent proxy can authorize it (allowlist mode).
	//
	// Without it, allowlist mode reaches only ProxyPort — mgit's
	// length-prefixed CONNECT protocol, which nothing in any guest speaks —
	// so no ordinary program could connect anywhere at all. Refs: MGIT-69
	TransparentPort int
}

// natChainName is the per-sandbox nat chain holding this tap's REDIRECT rule.
// Separate from the filter chain because nat and filter are distinct tables;
// a dedicated chain keeps teardown deterministic (flush + delete).
func (p TapPlan) natChainName() string {
	return "n" + p.TapDev // "n"+<=14 = <=15 chars, within iptables' 28-char limit
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

// chainName is the per-sandbox iptables chain holding this tap's filter
// rules. A dedicated chain — jumped to from the TOP of INPUT/FORWARD via
// -I ... 1 — guarantees mgit's rules take precedence over any pre-existing
// ACCEPT another tool installed (Docker, libvirt, a default-ACCEPT policy),
// and makes teardown deterministic (flush + delete the chain). Refs: SEC-04
func (p TapPlan) chainName() string {
	return "f" + p.TapDev // "f"+<=14 = <=15 chars, within iptables' 28-char limit
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

// allowlistRules give the guest no direct route: in a private chain reached
// from the top of INPUT and FORWARD, it may reach only the host proxy and
// the host resolver on the gateway; everything else is dropped and nothing
// is forwarded out (no NAT). The chain terminates in DROP, so a guest packet
// to any other destination is refused regardless of host-global rules.
// Refs: SEC-04, FR-17.8
func (p TapPlan) allowlistRules() [][]string {
	c, nc := p.chainName(), p.natChainName()
	gw := p.GatewayIP.String()
	proxy := fmt.Sprintf("%d", p.ProxyPort)
	dns := fmt.Sprintf("%d", p.DNSPort)
	transparent := fmt.Sprintf("%d", p.TransparentPort)
	return [][]string{
		// REDIRECT the guest's ordinary TCP into the transparent proxy, which
		// authorizes it on the original destination conntrack remembers. This
		// is what lets an unmodified program (npm, apt, curl, git) egress at
		// all in allowlist mode: before it, the only reachable TCP port was
		// mgit's length-prefixed CONNECT proxy, which nothing in any guest
		// speaks. The redirect changes HOW a flow reaches the authorizer, never
		// WHETHER policy decides it — the chain below still default-denies.
		// Refs: MGIT-69, SEC-04
		{"iptables", "-t", "nat", "-N", nc},
		// The gateway's OWN services (the CONNECT proxy, TCP DNS) are exempt:
		// redirecting them would make mgit's own endpoints unreachable from
		// the guest, and would have the transparent proxy authorize a flow
		// whose destination is the gateway — an RFC1918 address the
		// unconditional denials refuse. RETURN must precede the catch-all.
		{"iptables", "-t", "nat", "-A", nc, "-p", "tcp", "-d", gw, "-j", "RETURN"},
		{"iptables", "-t", "nat", "-A", nc, "-p", "tcp", "-j", "REDIRECT", "--to-ports", transparent},
		{"iptables", "-t", "nat", "-I", "PREROUTING", "1", "-i", p.TapDev, "-j", nc},

		{"iptables", "-N", c},
		// The redirected flow arrives at the gateway on the transparent port;
		// without this it would meet the chain's default DROP.
		{"iptables", "-A", c, "-p", "tcp", "-d", gw, "--dport", transparent, "-j", "ACCEPT"},
		{"iptables", "-A", c, "-p", "tcp", "-d", gw, "--dport", proxy, "-j", "ACCEPT"},
		{"iptables", "-A", c, "-p", "udp", "-d", gw, "--dport", dns, "-j", "ACCEPT"},
		{"iptables", "-A", c, "-p", "tcp", "-d", gw, "--dport", dns, "-j", "ACCEPT"},
		{"iptables", "-A", c, "-j", "DROP"}, // default-deny everything else
		{"iptables", "-I", "INPUT", "1", "-i", p.TapDev, "-j", c},
		{"iptables", "-I", "FORWARD", "1", "-i", p.TapDev, "-j", c},
	}
}

// openRules NAT the guest to the host network (full egress) — the explicitly
// risky posture (T3/T9 disabled). The forward-accept lives in the private
// chain (jumped from the top of FORWARD) and the return path + masquerade
// are inserted at the top of their chains. Refs: FR-17.7
func (p TapPlan) openRules() [][]string {
	c := p.chainName()
	gw := p.GatewayIP.String()
	dns := fmt.Sprintf("%d", p.DNSPort)
	return [][]string{
		{"sysctl", "-w", "net.ipv4.ip_forward=1"},
		{"iptables", "-N", c},
		{"iptables", "-A", c, "-o", p.ExtIface, "-j", "ACCEPT"},
		// Open mode binds a resolver on the gateway too (MGIT-69), so admit
		// the guest's DNS to it EXPLICITLY. Leaving it to the host's default
		// INPUT policy would make open-mode name resolution work or fail
		// depending on unrelated host firewall configuration.
		{"iptables", "-A", c, "-p", "udp", "-d", gw, "--dport", dns, "-j", "ACCEPT"},
		{"iptables", "-A", c, "-p", "tcp", "-d", gw, "--dport", dns, "-j", "ACCEPT"},
		{"iptables", "-I", "INPUT", "1", "-i", p.TapDev, "-j", c},
		{"iptables", "-I", "FORWARD", "1", "-i", p.TapDev, "-j", c},
		{"iptables", "-I", "FORWARD", "1", "-i", p.ExtIface, "-o", p.TapDev, "-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT"},
		{"iptables", "-t", "nat", "-I", "POSTROUTING", "1", "-s", p.GuestIP.String(), "-o", p.ExtIface, "-j", "MASQUERADE"},
	}
}

// TeardownCommands remove exactly what SetupCommands installed: the chain
// jumps, the flushed+deleted private chain, the open-mode return-path and nat
// rules, and finally the tap — leaving no host residue and no stale rule a
// future tap of the same (deterministic) name could inherit. Refs: FR-17.19, SEC-04
func (p TapPlan) TeardownCommands() [][]string {
	if p.Mode == model.NetworkModeNone || p.TapDev == "" {
		return nil
	}
	c := p.chainName()
	var cmds [][]string
	switch p.Mode {
	case model.NetworkModeAllowlist:
		nc := p.natChainName()
		cmds = append(cmds,
			[]string{"iptables", "-D", "INPUT", "-i", p.TapDev, "-j", c},
			[]string{"iptables", "-D", "FORWARD", "-i", p.TapDev, "-j", c},
			// A leftover REDIRECT would silently capture traffic if a tap name
			// were ever reused. Refs: FR-17.19, SEC-10
			[]string{"iptables", "-t", "nat", "-D", "PREROUTING", "-i", p.TapDev, "-j", nc},
			[]string{"iptables", "-t", "nat", "-F", nc},
			[]string{"iptables", "-t", "nat", "-X", nc},
		)
	case model.NetworkModeOpen:
		cmds = append(cmds,
			[]string{"iptables", "-t", "nat", "-D", "POSTROUTING", "-s", p.GuestIP.String(), "-o", p.ExtIface, "-j", "MASQUERADE"},
			[]string{"iptables", "-D", "FORWARD", "-i", p.ExtIface, "-o", p.TapDev, "-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT"},
			[]string{"iptables", "-D", "FORWARD", "-i", p.TapDev, "-j", c},
			[]string{"iptables", "-D", "INPUT", "-i", p.TapDev, "-j", c},
		)
	}
	cmds = append(cmds,
		[]string{"iptables", "-F", c},
		[]string{"iptables", "-X", c},
		[]string{"ip", "link", "del", p.TapDev},
	)
	return cmds
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
		// Fail closed rather than treating a missing redirect port as a
		// stricter posture: without it the guest reaches only a proxy protocol
		// it does not speak, which is a sandbox that cannot work, not a safer
		// one. Refs: MGIT-69
		if p.TransparentPort < 1 || p.TransparentPort > 65535 {
			return fmt.Errorf(
				"tap plan: allowlist mode needs a valid transparent redirect port, got %d",
				p.TransparentPort)
		}
	case model.NetworkModeOpen:
		if p.ExtIface == "" {
			return fmt.Errorf("tap plan: open mode needs an external interface to NAT through")
		}
	}
	return nil
}
