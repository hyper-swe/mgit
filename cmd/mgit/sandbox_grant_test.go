package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/controlproto"
	"github.com/hyper-swe/mgit/internal/model"
)

// TestSandboxGrants_ListsPending verifies `grants --task` prints each pending
// capability request with its host-observed destination and approval key.
// Refs: FR-17.12, MGIT-11.9.4
func TestSandboxGrants_ListsPending(t *testing.T) {
	fc := &fakeSandboxClient{pending: []controlproto.PendingGrant{
		{Capability: "egress", DestIP: "203.0.113.7", DestPort: 443, Key: "203.0.113.7:443"},
		{Capability: "egress", DestIP: "203.0.113.8", DestPort: 80, Key: "203.0.113.8:80"},
	}}

	out, err := runSandbox(okConnect(fc), "grants", "--task", "MGIT-1")
	require.NoError(t, err)
	assert.Equal(t, "MGIT-1", fc.grantsTID)
	assert.Contains(t, out, "203.0.113.7:443")
	assert.Contains(t, out, "203.0.113.8:80")
}

// TestSandboxGrants_Empty prints a human-readable empty notice. Refs: FR-17.12
func TestSandboxGrants_Empty(t *testing.T) {
	out, err := runSandbox(okConnect(&fakeSandboxClient{}), "grants", "--task", "MGIT-1")
	require.NoError(t, err)
	assert.Contains(t, out, "no pending capability requests")
}

// TestSandboxGrants_JSON emits the pending list as JSON for tooling. Refs: FR-17.12
func TestSandboxGrants_JSON(t *testing.T) {
	fc := &fakeSandboxClient{pending: []controlproto.PendingGrant{
		{Capability: "egress", DestIP: "203.0.113.7", DestPort: 443, Key: "203.0.113.7:443"},
	}}
	out, err := runSandbox(okConnect(fc), "grants", "--task", "MGIT-1", "--json")
	require.NoError(t, err)

	var got []controlproto.PendingGrant
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	require.Len(t, got, 1)
	assert.Equal(t, "203.0.113.7:443", got[0].Key)
}

// TestSandboxGrants_MissingTask rejects the call without --task. Refs: FR-17.12
func TestSandboxGrants_MissingTask(t *testing.T) {
	_, err := runSandbox(okConnect(&fakeSandboxClient{}), "grants")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--task")
}

// TestSandboxGrant_Approves verifies `grant --task <id> <key>` forwards the key
// and reports the granted destination. Refs: FR-17.12, SEC-05
func TestSandboxGrant_Approves(t *testing.T) {
	fc := &fakeSandboxClient{grantResult: &controlproto.GrantResult{
		Capability: "egress", DestIP: "203.0.113.7", DestPort: 443,
	}}

	out, err := runSandbox(okConnect(fc), "grant", "--task", "MGIT-1", "203.0.113.7:443")
	require.NoError(t, err)
	assert.Equal(t, "MGIT-1", fc.grantTID)
	assert.Equal(t, "203.0.113.7:443", fc.grantKey)
	assert.Contains(t, out, "203.0.113.7:443")
	assert.True(t, strings.Contains(out, "lifetime"), "tells the operator the grant is sandbox-scoped")
}

// TestSandboxGrant_MissingTask rejects approval without --task. Refs: FR-17.12
func TestSandboxGrant_MissingTask(t *testing.T) {
	_, err := runSandbox(okConnect(&fakeSandboxClient{}), "grant", "203.0.113.7:443")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--task")
}

// TestSandboxGrant_NoKey rejects approval without a key argument. Refs: FR-17.12
func TestSandboxGrant_NoKey(t *testing.T) {
	_, err := runSandbox(okConnect(&fakeSandboxClient{}), "grant", "--task", "MGIT-1")
	require.Error(t, err)
}

// TestSandboxGrant_UnknownKey surfaces the daemon's rejection of a key that is
// not pending. Refs: FR-17.12
func TestSandboxGrant_UnknownKey(t *testing.T) {
	fc := &fakeSandboxClient{opErr: model.ErrCapabilityGrantNotFound}
	_, err := runSandbox(okConnect(fc), "grant", "--task", "MGIT-1", "203.0.113.9:443")
	require.Error(t, err)
}
