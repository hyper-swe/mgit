// Package model tests verify the SandboxEvent type per MGIT-11.3.1
// acceptance criteria. Refs: FR-17.18
package model

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

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
