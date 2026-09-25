package model

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The record's JSON keys are the wire and the --json contract. The CLI's
// test round-trips through this same struct, so a renamed tag would pass
// there. The literal keys are pinned here. Refs: MGIT-231.1, MGIT-231
func TestSandboxInfo_LastBootFailure_HasItsWireKeys(t *testing.T) {
	b, err := json.Marshal(SandboxInfo{ID: "01JSB", TaskID: "T-1", LastBootFailure: &BootFailure{
		At: time.Date(2026, 9, 24, 10, 21, 3, 0, time.UTC), Cause: "boom"}})
	require.NoError(t, err)
	assert.Contains(t, string(b), `"last_boot_failure":{"at":"2026-09-24T10:21:03Z","cause":"boom"}`)
	b, err = json.Marshal(SandboxInfo{ID: "01JSB", TaskID: "T-1"})
	require.NoError(t, err)
	assert.NotContains(t, string(b), "last_boot_failure", "omitted, not null, when there is none")
}
