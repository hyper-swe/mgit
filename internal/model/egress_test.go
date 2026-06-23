// Package model tests verify the EgressRecord type per MGIT-11.3.5
// acceptance criteria. Refs: FR-17.8, FR-17.18
package model

import (
	"strings"
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
		{name: "oversized_dest_host", mutate: func(e *EgressRecord) { e.DestHost = strings.Repeat("a", MaxEgressDestHostLen+1) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := valid
			tt.mutate(&rec)
			assert.Error(t, rec.Validate())
		})
	}
}

// TestEgressRecord_Validate_DestHostBound proves the model boundary caps the
// guest-influenced DestHost length (defense-in-depth, consistent with the
// store's sanitize cap): a host at the cap validates, one byte over is
// rejected before it can ever enter the append-only log. Refs: FR-17.8, F-09
func TestEgressRecord_Validate_DestHostBound(t *testing.T) {
	valid := EgressRecord{
		SandboxID: "01JX", TaskID: "MGIT-4.2",
		Decision: EgressAllow, Protocol: "tcp", DestPort: 443,
	}

	atCap := valid
	atCap.DestHost = strings.Repeat("a", MaxEgressDestHostLen)
	assert.NoError(t, atCap.Validate(), "a DestHost exactly at the cap is accepted")

	overCap := valid
	overCap.DestHost = strings.Repeat("a", MaxEgressDestHostLen+1)
	assert.Error(t, overCap.Validate(), "a DestHost one byte over the cap is rejected")
}

// TestTruncateDestHost proves an over-cap host is truncated to a valid length
// (so an audit builder can record a hostile over-cap name without Validate
// rejecting it), while a within-cap host is returned unchanged. Refs: FR-17.8, F-09
func TestTruncateDestHost(t *testing.T) {
	short := "registry.npmjs.org"
	assert.Equal(t, short, TruncateDestHost(short), "a within-cap host is unchanged")

	over := strings.Repeat("a", MaxEgressDestHostLen+50)
	got := TruncateDestHost(over)
	assert.LessOrEqual(t, len(got), MaxEgressDestHostLen, "an over-cap host is truncated to the cap")

	// A truncated host produces a record that passes Validate.
	rec := EgressRecord{
		SandboxID: "01JX", TaskID: "MGIT-4.2",
		Decision: EgressDeny, Protocol: "tcp", DestHost: got, DestPort: 443,
	}
	assert.NoError(t, rec.Validate(), "a truncated host yields a valid auditable record")
}
