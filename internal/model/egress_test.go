// Package model tests verify the EgressRecord type per MGIT-11.3.5
// acceptance criteria. Refs: FR-17.8, FR-17.18
package model

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestEgressRecord_Validate covers the decision vocabulary and shape
// checks. Refs: FR-17.8
func TestEgressRecord_Validate(t *testing.T) {
	valid := EgressRecord{
		SandboxID: "01JX", TaskID: "MGIT-4.2",
		Decision: EgressAllow, Protocol: "tcp",
		DestHost: "proxy.golang.org", DestIP: "142.250.4.141", DestPort: 443,
		Rule: "proxy.golang.org",
	}
	assert.NoError(t, valid.Validate())

	t.Run("deny_valid", func(t *testing.T) {
		rec := valid
		rec.Decision = EgressDeny
		rec.Rule = "denied: metadata endpoint"
		assert.NoError(t, rec.Validate())
	})

	t.Run("port_zero_valid_for_dns", func(t *testing.T) {
		rec := valid
		rec.Protocol, rec.DestPort = "dns", 0
		assert.NoError(t, rec.Validate())
	})

	tests := []struct {
		name   string
		mutate func(*EgressRecord)
	}{
		{name: "empty_sandbox_id", mutate: func(e *EgressRecord) { e.SandboxID = "" }},
		{name: "empty_task_id", mutate: func(e *EgressRecord) { e.TaskID = "" }},
		{name: "malformed_task_id", mutate: func(e *EgressRecord) { e.TaskID = "nope!" }},
		{name: "unknown_decision", mutate: func(e *EgressRecord) { e.Decision = "maybe" }},
		{name: "empty_decision", mutate: func(e *EgressRecord) { e.Decision = "" }},
		{name: "empty_protocol", mutate: func(e *EgressRecord) { e.Protocol = "" }},
		{name: "negative_port", mutate: func(e *EgressRecord) { e.DestPort = -1 }},
		{name: "oversized_port", mutate: func(e *EgressRecord) { e.DestPort = 70000 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := valid
			tt.mutate(&rec)
			assert.Error(t, rec.Validate())
		})
	}
}
