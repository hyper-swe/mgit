package reviewbind

import (
	"errors"
	"regexp"
	"strings"
)

// EgressKind is what a policy allows.
type EgressKind int

// The two policies: nothing, or exactly one named host.
const (
	EgressNone EgressKind = iota
	EgressHost
)

// Policy is a parsed egress policy id.
type Policy struct {
	Kind EgressKind
	Host string
}

var hostPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)
var ipLike = regexp.MustCompile(`^[0-9.]+$`)

// ParseEgress reads a policy id: "" or "none", or "host:<name>" with one
// fully qualified DNS name. No wildcard, no address, no list, no port.
func ParseEgress(id string) (Policy, error) {
	if id == "" || id == "none" {
		return Policy{Kind: EgressNone}, nil
	}
	h, ok := strings.CutPrefix(id, "host:")
	if !ok || !hostPattern.MatchString(h) || ipLike.MatchString(h) || h == "localhost" {
		return Policy{}, errors.New("egress policy must be none or host:<one fully qualified name>")
	}
	return Policy{Kind: EgressHost, Host: h}, nil
}

// Allows reports whether a connection to host is inside the policy.
func (p Policy) Allows(host string) bool {
	return p.Kind == EgressHost && strings.EqualFold(host, p.Host)
}
