// Package model tests verify the SandboxEvent type per MGIT-11.3.1
// acceptance criteria. Refs: FR-17.18
package model

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestStateForEvent_Mapping covers the event->state derivation map.
// Refs: FR-17.18
func TestStateForEvent_Mapping(t *testing.T) {
	tests := []struct {
		eventType string
		wantState string
		wantOK    bool
	}{
		{eventType: EventCreated, wantState: StateCreated, wantOK: true},
		{eventType: EventSuspended, wantState: StateSuspended, wantOK: true},
		{eventType: EventResumed, wantState: StateRunning, wantOK: true},
		{eventType: EventLanded, wantState: StateLanded, wantOK: true},
		{eventType: EventDestroyed, wantState: StateDestroyed, wantOK: true},
		{eventType: EventTTLExpired, wantState: StateDestroyed, wantOK: true},
		{eventType: EventKilled, wantState: StateDestroyed, wantOK: true},
		{eventType: EventPolicyGranted, wantState: "", wantOK: false},
		{eventType: "rebooted", wantState: "", wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.eventType, func(t *testing.T) {
			state, ok := StateForEvent(tt.eventType)
			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.wantState, state)
		})
	}
}

// TestSandboxEvent_Validate covers the closed event-type vocabulary
// and optional-field validation. Refs: FR-17.18
func TestSandboxEvent_Validate(t *testing.T) {
	valid := SandboxEvent{
		SandboxID:   "01JX",
		TaskID:      "MGIT-4.2",
		EventType:   EventCreated,
		Backend:     BackendKVM,
		ImageDigest: "sha256:" + strings.Repeat("a", 64),
		NetworkMode: NetworkModeAllowlist,
	}
	assert.NoError(t, valid.Validate())

	t.Run("all_event_types_valid", func(t *testing.T) {
		for _, eventType := range []string{
			EventCreated, EventSuspended, EventResumed, EventPolicyGranted,
			EventLanded, EventDestroyed, EventTTLExpired, EventKilled,
		} {
			ev := valid
			ev.EventType = eventType
			assert.NoError(t, ev.Validate(), "event type %q must validate", eventType)
		}
	})

	t.Run("optional_fields_may_be_empty", func(t *testing.T) {
		ev := SandboxEvent{SandboxID: "01JX", TaskID: "MGIT-4.2", EventType: EventDestroyed}
		assert.NoError(t, ev.Validate())
	})

	tests := []struct {
		name   string
		mutate func(*SandboxEvent)
	}{
		{name: "empty_sandbox_id", mutate: func(e *SandboxEvent) { e.SandboxID = "" }},
		{name: "empty_task_id", mutate: func(e *SandboxEvent) { e.TaskID = "" }},
		{name: "malformed_task_id", mutate: func(e *SandboxEvent) { e.TaskID = "not a task!" }},
		{name: "unknown_event_type", mutate: func(e *SandboxEvent) { e.EventType = "rebooted" }},
		{name: "empty_event_type", mutate: func(e *SandboxEvent) { e.EventType = "" }},
		{name: "unknown_backend", mutate: func(e *SandboxEvent) { e.Backend = "qemu" }},
		{name: "malformed_image_digest", mutate: func(e *SandboxEvent) { e.ImageDigest = "sha256:short" }},
		{name: "unknown_network_mode", mutate: func(e *SandboxEvent) { e.NetworkMode = "nat" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev := valid
			tt.mutate(&ev)
			assert.Error(t, ev.Validate())
		})
	}
}

// TestSandboxEvent_PolicyChanged_IsValidAndAuditOnly verifies the live
// egress-policy mutation event is in the closed vocabulary and carries NO
// lifecycle state change.
//
// A runtime-mutable policy with no audit record is worse than one that cannot
// change at all — a reviewer could no longer reconstruct what was being
// enforced when. And it must not be state-bearing: a revoke does not suspend,
// destroy or resume a sandbox, and deriving state from it would corrupt the
// sandbox's status. Refs: MGIT-72, FR-17.18
func TestSandboxEvent_PolicyChanged_IsValidAndAuditOnly(t *testing.T) {
	ev := SandboxEvent{
		SandboxID: "01JX", TaskID: "MGIT-72", EventType: EventPolicyChanged,
		Detail: `{"entries":[],"killed":1}`,
	}
	if err := ev.Validate(); err != nil {
		t.Fatalf("policy_changed must be a valid event type: %v", err)
	}
	if _, stateBearing := StateForEvent(EventPolicyChanged); stateBearing {
		t.Error("policy_changed must not derive a sandbox state — a revoke is not a lifecycle transition")
	}
	var listed bool
	for _, t := range NonStateEventTypes() {
		if t == EventPolicyChanged {
			listed = true
		}
	}
	if !listed {
		t.Error("policy_changed must be listed as audit-only so state derivation skips past it")
	}
}
