package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// `sandbox status` must tell a sandbox nobody has used from one whose first
// boot failed, in both of its forms: the human line says the boot failed,
// when, and why; --json carries the same record. Refs: MGIT-231
func TestSandboxStatus_AFailedBootIsNamed_PlainAndJSON(t *testing.T) {
	at := time.Date(2026, 9, 24, 10, 21, 3, 0, time.UTC)
	const cause = `sandbox ensure-running: kvm launch: create vm: firecracker config invalid: failed to stat kernel image path, ""`
	failed := &model.SandboxInfo{ID: "01JSB", TaskID: "WALK-1", State: model.StateCreated,
		LastBootFailure: &model.BootFailure{At: at, Cause: cause}}

	out, err := runSandbox(okConnect(&fakeSandboxClient{statusInfo: failed}), "status", "WALK-1")
	require.NoError(t, err)
	assert.Contains(t, out, "WALK-1\tcreated\t01JSB")
	assert.Contains(t, out, "last boot FAILED at 2026-09-24T10:21:03Z")
	assert.Contains(t, out, "failed to stat kernel image path")

	out, err = runSandbox(okConnect(&fakeSandboxClient{statusInfo: failed}), "status", "WALK-1", "--json")
	require.NoError(t, err)
	var got model.SandboxInfo
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	require.NotNil(t, got.LastBootFailure, "--json carries the record: %s", out)
	assert.Equal(t, at, got.LastBootFailure.At)
	assert.Equal(t, cause, got.LastBootFailure.Cause)
	assert.Contains(t, out, `"last_boot_failure"`)

	fresh := &model.SandboxInfo{ID: "01JSB", TaskID: "WALK-1", State: model.StateCreated}
	out, err = runSandbox(okConnect(&fakeSandboxClient{statusInfo: fresh}), "status", "WALK-1")
	require.NoError(t, err)
	assert.NotContains(t, out, "FAILED", "a sandbox never tried says nothing about a failure")
	out, err = runSandbox(okConnect(&fakeSandboxClient{statusInfo: fresh}), "status", "WALK-1", "--json")
	require.NoError(t, err)
	assert.NotContains(t, out, "last_boot_failure", "absent, not null, when there is none")
}
