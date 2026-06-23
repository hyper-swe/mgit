package egress

import (
	"fmt"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// hostnameRe validates one DNS hostname (the apex of a name rule or the
// suffix labels of a wildcard). Lowercase labels only — the allowlist
// grammar (model.allowlistEntryRe) already excludes uppercase and control
// characters; this rejects the residue (spaces, empty labels) so a
// malformed entry fails closed at compile time rather than matching by
// accident. Refs: SEC-04, FR-17.8
var hostnameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)

// nameRule authorizes a destination by DNS name. A wildcard rule matches
// proper subdomains of suffix (".golang.org" matches "a.golang.org" but
// not the apex "golang.org"); an exact rule matches one name. port 0 means
// any port.
type nameRule struct {
	exact    string // full name for an exact rule ("registry.npmjs.org")
	suffix   string // ".golang.org" for a wildcard rule; empty for exact
	port     int    // 0 = any port
	wildcard bool
}

// netRule authorizes a destination by IP. It matches a connection whose
// resolved/literal destination IP is inside prefix. port 0 means any port.
type netRule struct {
	prefix netip.Prefix
	port   int
}

// Allowlist is the compiled, immutable host egress allowlist. It answers
// three host-side questions: may this NAME resolve (HasName, the resolver
// gate), may a resolved name reach this destination (AllowsName), and may
// a raw-IP connection proceed (AllowsIP). It never widens with guest
// input. Refs: SEC-04, FR-17.8
type Allowlist struct {
	names []nameRule // immutable after Compile
	nets  []netRule  // immutable after Compile

	// grants holds host-approved live additions (scoped to the sandbox
	// lifetime). Guarded by mu because AllowsIP runs concurrently per guest
	// flow while a grant may be added/revoked. Each entry is one exact
	// (ip, port) — never a range — so a grant authorizes only the one
	// host-observed destination it names (SEC-05). Refs: FR-17.12, SEC-05
	mu     sync.RWMutex
	grants map[grantKey]struct{}
}

// grantKey identifies one exact host-approved granted destination.
type grantKey struct {
	ip   netip.Addr
	port int
}

// GrantIP adds a host-approved live grant for exactly one (ip, port). It is
// the live-enforcement half of a sandbox-lifetime capability grant: the
// allowlist now admits this single destination until RevokeGrants drops it.
// The ip must be valid and the port a real TCP port; a range is never
// accepted (no allow-all, SEC-05). Refs: FR-17.12, SEC-05
func (al *Allowlist) GrantIP(ip netip.Addr, port int) error {
	if !ip.IsValid() {
		return fmt.Errorf("egress grant: invalid ip")
	}
	if port < 1 || port > 65535 {
		return fmt.Errorf("egress grant: invalid port %d", port)
	}
	al.mu.Lock()
	defer al.mu.Unlock()
	if al.grants == nil {
		al.grants = make(map[grantKey]struct{})
	}
	al.grants[grantKey{ip: ip.Unmap(), port: port}] = struct{}{}
	return nil
}

// RevokeGrants drops every live grant. Called on sandbox teardown so a grant
// is scoped to the sandbox lifetime and never outlives it. The launch-time
// allowlist is untouched. Refs: FR-17.12, SEC-05
func (al *Allowlist) RevokeGrants() {
	al.mu.Lock()
	defer al.mu.Unlock()
	al.grants = nil
}

// isGranted reports whether an exact (ip, port) was live-granted.
func (al *Allowlist) isGranted(ip netip.Addr, port int) bool {
	al.mu.RLock()
	defer al.mu.RUnlock()
	if al.grants == nil {
		return false
	}
	_, ok := al.grants[grantKey{ip: ip.Unmap(), port: port}]
	return ok
}

// Compile builds an Allowlist from validated policy entries (each already
// matched model.allowlistEntryRe). A match-all wildcard ("*", "*.") and
// any entry that is neither a valid IP/CIDR nor a valid hostname is
// rejected — fail closed, no allow-all (SEC-04). Refs: SEC-04, FR-17.8
func Compile(entries []string) (*Allowlist, error) {
	al := &Allowlist{}
	for _, raw := range entries {
		if err := al.add(raw); err != nil {
			return nil, fmt.Errorf("allowlist entry %q: %w", raw, err)
		}
	}
	return al, nil
}

// add classifies and appends one entry as an IP/CIDR rule or a name rule.
func (al *Allowlist) add(raw string) error {
	if raw == "" {
		return fmt.Errorf("empty entry")
	}
	// CIDR: the only entry form carrying a '/'.
	if strings.Contains(raw, "/") {
		prefix, err := netip.ParsePrefix(raw)
		if err != nil {
			return fmt.Errorf("invalid CIDR: %w", err)
		}
		al.nets = append(al.nets, netRule{prefix: prefix.Masked()})
		return nil
	}
	// Bare IP literal (IPv4 or IPv6), no port.
	if ip, err := netip.ParseAddr(raw); err == nil {
		al.nets = append(al.nets, netRule{prefix: netip.PrefixFrom(ip, ip.BitLen())})
		return nil
	}
	host, port, err := splitOptionalPort(raw)
	if err != nil {
		return err
	}
	// host:port where host is an IP literal.
	if ip, err := netip.ParseAddr(host); err == nil {
		al.nets = append(al.nets, netRule{prefix: netip.PrefixFrom(ip, ip.BitLen()), port: port})
		return nil
	}
	return al.addName(host, port)
}

// addName appends a wildcard or exact hostname rule, rejecting a match-all.
func (al *Allowlist) addName(host string, port int) error {
	if suffix, ok := strings.CutPrefix(host, "*."); ok {
		if suffix == "" || !hostnameRe.MatchString(suffix) {
			return fmt.Errorf("wildcard must name a domain (e.g. *.example.com), not match-all")
		}
		al.names = append(al.names, nameRule{suffix: "." + suffix, port: port, wildcard: true})
		return nil
	}
	if !hostnameRe.MatchString(host) {
		return fmt.Errorf("not a valid hostname")
	}
	al.names = append(al.names, nameRule{exact: host, port: port})
	return nil
}

// splitOptionalPort splits a trailing ":<port>" off a non-IP entry. The
// port, when present, must be a valid 1-65535 TCP port. (IPv6 literals are
// handled by the caller before this point, so a remaining colon here is a
// host:port separator.)
func splitOptionalPort(raw string) (string, int, error) {
	host, portStr, found := strings.Cut(raw, ":")
	if !found {
		return raw, 0, nil
	}
	if strings.Contains(portStr, ":") {
		return "", 0, fmt.Errorf("malformed host:port")
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return "", 0, fmt.Errorf("invalid port %q", portStr)
	}
	return host, port, nil
}

// AllowsName reports whether a resolved allowlisted host may be reached on
// port. The host is compared case-insensitively (the guest may upper-case).
// Refs: SEC-04, FR-17.8
func (al *Allowlist) AllowsName(host string, port int) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, r := range al.names {
		if r.matches(host) && portOK(r.port, port) {
			return true
		}
	}
	return false
}

// HasName reports whether a name is allowlisted at all (ignoring port).
// The host-side resolver consults this so only allowlisted names resolve
// (SEC-07 anti-tunnel). Refs: SEC-07, FR-17.8
func (al *Allowlist) HasName(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, r := range al.names {
		if r.matches(host) {
			return true
		}
	}
	return false
}

// AllowsIP reports whether a raw-IP / literal destination is authorized by
// an IP or CIDR entry. A plain hostname entry authorizes no raw IP — a
// raw-IP connection bypasses host-side DNS, so it is only permitted when an
// IP/CIDR entry names it (SEC-04 raw-IP bypass defense). Refs: SEC-04
func (al *Allowlist) AllowsIP(ip netip.Addr, port int) bool {
	ip = ip.Unmap()
	for _, r := range al.nets {
		if r.prefix.Contains(ip) && portOK(r.port, port) {
			return true
		}
	}
	// A host-approved live grant (SEC-05) admits exactly this (ip, port). The
	// authorizer still applies the unconditional denied-range gate first, so a
	// grant can never re-open a denied range. Refs: FR-17.12, SEC-05
	return al.isGranted(ip, port)
}

// matches reports whether a hostname satisfies this name rule.
func (r nameRule) matches(host string) bool {
	if r.wildcard {
		return strings.HasSuffix(host, r.suffix) && len(host) > len(r.suffix)
	}
	return host == r.exact
}

// portOK reports whether a rule port (0 = any) admits the request port.
func portOK(rulePort, reqPort int) bool {
	return rulePort == 0 || rulePort == reqPort
}
