package model

import (
	"fmt"
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

// validEgressDecisions closes the decision vocabulary.
var validEgressDecisions = map[string]bool{EgressAllow: true, EgressDeny: true}

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
	if e.Protocol == "" {
		return &ValidationError{Field: "protocol", Message: "must not be empty"}
	}
	if e.DestPort < 0 || e.DestPort > 65535 {
		return &ValidationError{Field: "dest_port", Message: "must be 0-65535"}
	}
	return nil
}
