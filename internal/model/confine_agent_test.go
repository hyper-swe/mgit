package model

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestConfineAgent_DefaultsOff verifies T2 is opt-in: the default policy
// and a bare launch leave confine_agent off (T1). Refs: MGIT-11.11.4
func TestConfineAgent_DefaultsOff(t *testing.T) {
	assert.False(t, DefaultSandboxPolicy().ConfineAgent, "default topology is T1")
	assert.False(t, SandboxLaunchOptions{}.ConfineAgent, "a bare launch is T1")
}

// TestSessionCredential_ValueNeverSerialized verifies a credential value is
// never emitted by JSON encoding — only its name — so it cannot leak into
// the audit log or any serialized image config. Refs: MGIT-11.11.4
func TestSessionCredential_ValueNeverSerialized(t *testing.T) {
	c := SessionCredential{Name: "ANTHROPIC_API_KEY", Value: "sk-secret-123"}
	b, err := json.Marshal(c)
	require.NoError(t, err)
	assert.NotContains(t, string(b), "sk-secret-123", "the secret value is never serialized")
	assert.Contains(t, string(b), "ANTHROPIC_API_KEY", "the name is retained")
}

// TestRedactedCredentialNames_OmitsValues verifies the audit projection is
// names only. Refs: MGIT-11.11.4
func TestRedactedCredentialNames_OmitsValues(t *testing.T) {
	creds := []SessionCredential{
		{Name: "ANTHROPIC_API_KEY", Value: "sk-1"},
		{Name: "GH_TOKEN", Value: "ghp_2"},
	}
	names := RedactedCredentialNames(creds)
	assert.Equal(t, []string{"ANTHROPIC_API_KEY", "GH_TOKEN"}, names)
	joined := strings.Join(names, ",")
	assert.NotContains(t, joined, "sk-1")
	assert.NotContains(t, joined, "ghp_2")
}

// TestEventCredentialsInjected_IsAuditOnly verifies the new event type is a
// valid audit event but carries NO lifecycle state transition (like
// policy_granted). Refs: MGIT-11.11.4, FR-17.18
func TestEventCredentialsInjected_IsAuditOnly(t *testing.T) {
	ev := SandboxEvent{SandboxID: "01JX", TaskID: "MGIT-4.2", EventType: EventCredentialsInjected}
	assert.NoError(t, ev.Validate())
	_, stateBearing := StateForEvent(EventCredentialsInjected)
	assert.False(t, stateBearing, "credential injection is an audit event, not a state transition")
}
