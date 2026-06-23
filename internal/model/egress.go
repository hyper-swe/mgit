package model

import (
	"fmt"
	"net/netip"
	"strings"
	"time"
)

// Egress decisions recorded by the host proxy in allowlist mode.
// Every allow and deny is appended to the audit log (FR-17.8).
const (
	// EgressAllow records a permitted flow to an allowlisted resolved IP.
	EgressAllow = "allow"
	// EgressDeny records a refused flow (and why, in Rule).
	EgressDeny = "deny"
)

// MaxEgressDestHostLen caps the guest-influenced DestHost length at the model
// boundary (defense-in-depth). It mirrors the store's sanitize cap so an
// oversized host is rejected before it can ever reach the append-only log,
// regardless of which path constructs the record. 255 is the maximum DNS name
// length (RFC 1035) and matches index.maxEgressHostLen. Refs: FR-17.8, F-09
const MaxEgressDestHostLen = 255

// validEgressDecisions closes the decision vocabulary.
var validEgressDecisions = map[string]bool{EgressAllow: true, EgressDeny: true}

// validEgressProtocols closes the protocol vocabulary: the allowlist
// proxy permits TCP, host-resolver DNS, and (denied) UDP — nothing
// else exists at the flow layer (FR-17.8).
var validEgressProtocols = map[string]bool{"tcp": true, "udp": true, "dns": true}

// EgressRecord is one append-only proxy decision. DestHost and Rule
// originate guest-side (hostnames, SNI) and are sanitized and
// length-capped at the store boundary (SEC-04, SEC-07, F-09). ID and
// CreatedAt are store-assigned. Refs: FR-17.8, FR-17.18
type EgressRecord struct {
	ID        string    `json:"id"` // ULID, store-assigned
	SandboxID string    `json:"sandbox_id"`
	TaskID    string    `json:"task_id"`
	Decision  string    `json:"decision"`          // allow | deny
	Protocol  string    `json:"protocol"`          // tcp | udp | dns
	DestHost  string    `json:"dest_host"`         // guest-influenced; sanitized
	DestIP    string    `json:"dest_ip,omitempty"` // host-resolved destination
	DestPort  int       `json:"dest_port"`         // 0 when not applicable (dns)
	Rule      string    `json:"rule"`              // matched allowlist entry or deny reason
	CreatedAt time.Time `json:"created_at"`        // ISO-8601 UTC, store-assigned
}

// TruncateDestHost caps a guest-influenced host string to MaxEgressDestHostLen
// bytes (UTF-8-safe: a split trailing rune is dropped), for audit-record
// builders that must record a hostile, possibly over-cap name without the
// record being rejected by Validate. The store separately strips control
// characters; this only bounds the length so "every deny is audited" (FR-17.8)
// holds even for an over-cap name. Refs: FR-17.8, F-09
func TruncateDestHost(host string) string {
	if len(host) <= MaxEgressDestHostLen {
		return host
	}
	return strings.ToValidUTF8(host[:MaxEgressDestHostLen], "")
}

// Validate checks the record shape before it enters the append-only
// log. Refs: FR-17.8, FR-17.18
func (e EgressRecord) Validate() error {
	if e.SandboxID == "" {
		return &ValidationError{Field: "sandbox_id", Message: "must not be empty"}
	}
	if err := validateTaskIDField(e.TaskID); err != nil {
		return err
	}
	if !validEgressDecisions[e.Decision] {
		return &ValidationError{Field: "decision", Message: fmt.Sprintf("unknown decision %q", e.Decision)}
	}
	if !validEgressProtocols[e.Protocol] {
		return &ValidationError{Field: "protocol", Message: fmt.Sprintf("unknown protocol %q", e.Protocol)}
	}
	// Cap the guest-influenced host length at the model boundary (the store
	// also sanitizes/truncates, but this rejects an oversized host before it
	// reaches any sink — defense-in-depth, F-09).
	if len(e.DestHost) > MaxEgressDestHostLen {
		return &ValidationError{Field: "dest_host", Message: fmt.Sprintf("must be at most %d bytes", MaxEgressDestHostLen)}
	}
	if e.DestIP != "" {
		if _, err := netip.ParseAddr(e.DestIP); err != nil {
			return &ValidationError{Field: "dest_ip", Message: "must be an IP address when set"}
		}
	}
	if e.DestPort < 0 || e.DestPort > 65535 {
		return &ValidationError{Field: "dest_port", Message: "must be 0-65535"}
	}
	return nil
}
