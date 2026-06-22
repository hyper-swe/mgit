package model

import (
	"fmt"
	"time"
)

// Sandbox lifecycle event types, the closed vocabulary of the
// event-sourced sandbox_events audit table. Sandbox state is derived
// from the latest event — transitions append, never mutate (F-01).
// Refs: FR-17.18
const (
	// EventCreated records sandbox registration (lazy: VM may not be booted).
	EventCreated = "created"
	// EventSuspended records an idle-suspend pause (NFR-17.3).
	EventSuspended = "suspended"
	// EventResumed records a resume from suspension.
	EventResumed = "resumed"
	// EventPolicyGranted records a capability grant (FR-17.12).
	EventPolicyGranted = "policy_granted"
	// EventCredentialsInjected records that per-session credentials were
	// injected into a T2 confined-agent guest (MGIT-11.11.4). Audit-only
	// (no state change); Detail carries credential NAMES only, never values.
	EventCredentialsInjected = "credentials_injected"
	// EventLanded records a verified land import (FR-17.5).
	EventLanded = "landed"
	// EventDestroyed records teardown.
	EventDestroyed = "destroyed"
	// EventTTLExpired records TTL-based reaping (FR-17.9).
	EventTTLExpired = "ttl_expired"
	// EventKilled records a forced stop.
	EventKilled = "killed"
)

// isValidEventType closes the vocabulary so audit writers cannot fork
// it with typos. Every state-bearing event (eventStates key) is valid,
// plus the one audit-only event, policy_granted.
func isValidEventType(eventType string) bool {
	_, stateBearing := eventStates[eventType]
	return stateBearing || eventType == EventPolicyGranted || eventType == EventCredentialsInjected
}

// eventStates maps each state-bearing event type to the sandbox state
// it produces. policy_granted is deliberately absent: capability
// grants are audit events, not lifecycle transitions.
var eventStates = map[string]string{
	EventCreated:    StateCreated,
	EventSuspended:  StateSuspended,
	EventResumed:    StateRunning,
	EventLanded:     StateLanded,
	EventDestroyed:  StateDestroyed,
	EventTTLExpired: StateDestroyed,
	EventKilled:     StateDestroyed,
}

// StateForEvent maps a lifecycle event type to the sandbox state it
// produces. ok is false for event types that carry no state change
// (policy_granted) or are unknown. Refs: FR-17.18
func StateForEvent(eventType string) (string, bool) {
	state, ok := eventStates[eventType]
	return state, ok
}

// SandboxEvent is one append-only sandbox lifecycle record. ID and
// CreatedAt are store-assigned at append time. Detail carries
// event-specific JSON; guest-sourced strings inside it are sanitized
// and length-capped at the store boundary (F-09). Refs: FR-17.18
type SandboxEvent struct {
	ID          string    `json:"id"` // ULID, store-assigned (sortable: event order)
	SandboxID   string    `json:"sandbox_id"`
	TaskID      string    `json:"task_id"`
	EventType   string    `json:"event_type"`
	Backend     string    `json:"backend,omitempty"`      // created event
	ImageDigest string    `json:"image_digest,omitempty"` // created event
	NetworkMode string    `json:"network_mode,omitempty"` // created/policy events
	Detail      string    `json:"detail,omitempty"`       // JSON; sanitized + capped
	CreatedAt   time.Time `json:"created_at"`             // ISO-8601 UTC, store-assigned
}

// Validate checks the event shape before it enters the append-only
// table — invalid rows would be permanent. Optional fields (backend,
// image digest, network mode) are validated when present.
// Refs: FR-17.18
func (e SandboxEvent) Validate() error {
	if e.SandboxID == "" {
		return &ValidationError{Field: "sandbox_id", Message: "must not be empty"}
	}
	if err := validateTaskIDField(e.TaskID); err != nil {
		return err
	}
	if !isValidEventType(e.EventType) {
		return &ValidationError{Field: "event_type", Message: fmt.Sprintf("unknown event type %q", e.EventType)}
	}
	if e.Backend != "" && !validBackends[e.Backend] {
		return &ValidationError{Field: "backend", Message: fmt.Sprintf("unknown backend %q", e.Backend)}
	}
	if e.ImageDigest != "" && !sha256DigestRe.MatchString(e.ImageDigest) {
		return &ValidationError{Field: "image_digest", Message: "must be sha256:<64 hex>"}
	}
	if e.NetworkMode != "" {
		if err := (NetworkPolicy{Mode: e.NetworkMode}).Validate(); err != nil {
			return nestField("network", err)
		}
	}
	return nil
}
