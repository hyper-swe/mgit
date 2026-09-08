package model

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// An explicit root escalation of a guest exec is an audit event of its
// own: valid, and audit-only (it moves the sandbox to no state).
// Refs: MGIT-151, FR-17.18
func TestEventExecPrivileged_IsAnAuditOnlyEvent(t *testing.T) {
	assert.Equal(t, "exec_privileged", EventExecPrivileged)
	ev := SandboxEvent{SandboxID: "sb", TaskID: "MGIT-151", EventType: EventExecPrivileged}
	assert.NoError(t, ev.Validate(), "the type is in the vocabulary")
	assert.Contains(t, NonStateEventTypes(), EventExecPrivileged, "an escalation changes no lifecycle state")
}
