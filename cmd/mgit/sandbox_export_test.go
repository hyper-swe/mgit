package main

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

func TestSandboxExport_NamesBothPathsAndReportsWhatCrossed(t *testing.T) {
	fc := &fakeSandboxClient{}
	dest := filepath.Join(t.TempDir(), "node_modules")

	out, err := runSandbox(okConnect(fc), "export", "--task", "MGIT-73", "node_modules", dest)

	require.NoError(t, err)
	assert.Equal(t, "MGIT-73", fc.exportTID)
	assert.Equal(t, "node_modules", fc.exportReq.GuestPath, "the guest path stays worktree-relative")
	assert.Equal(t, canonicalPath(dest), fc.exportReq.HostPath,
		"the host destination is resolved absolute before it reaches the daemon")
	assert.Contains(t, out, "Exported node_modules -> ")
	assert.Contains(t, out, "3 file(s), 4096 bytes")
	assert.Contains(t, out, "Provenance:", "the operator is told where the provenance record landed")
}

func TestSandboxExport_JSON_EmitsTheFullRecord(t *testing.T) {
	fc := &fakeSandboxClient{}
	dest := filepath.Join(t.TempDir(), "artifact")

	out, err := runSandbox(okConnect(fc), "export", "--task", "MGIT-73", "--json", "build", dest)

	require.NoError(t, err)
	var res model.ArtifactExportResult
	require.NoError(t, json.Unmarshal([]byte(out), &res))
	assert.Equal(t, "build", res.GuestPath)
	assert.Equal(t, "abc123", res.TreeHash)
	assert.NotEmpty(t, res.ManifestPath)
}

func TestSandboxExport_MissingTask_IsRefused(t *testing.T) {
	fc := &fakeSandboxClient{}

	_, err := runSandbox(okConnect(fc), "export", "build", filepath.Join(t.TempDir(), "artifact"))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "--task-id is required")
	assert.Empty(t, fc.exportTID, "no daemon call is made without a task binding")
}

func TestSandboxExport_MissingPositionalArgs_IsRefused(t *testing.T) {
	fc := &fakeSandboxClient{}

	_, err := runSandbox(okConnect(fc), "export", "--task", "MGIT-73", "build")

	require.Error(t, err, "export needs BOTH a guest path and a host destination")
	assert.Empty(t, fc.exportTID)
}

func TestSandboxExport_DaemonRefusal_SurfacesTheReason(t *testing.T) {
	fc := &fakeSandboxClient{opErr: errors.New("artifact export path is unsafe: symlink escapes")}

	_, err := runSandbox(okConnect(fc), "export", "--task", "MGIT-73", "build",
		filepath.Join(t.TempDir(), "artifact"))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsafe", "a refusal must name its reason, not just fail")
}

func TestSandboxExport_HelpStatesTheRefusalsAndTheProvenance(t *testing.T) {
	out, err := runSandbox(okConnect(&fakeSandboxClient{}), "export", "--help")

	require.NoError(t, err)
	for _, want := range []string{"already exists", "symlink", "provenance sidecar", "audit"} {
		assert.True(t, strings.Contains(out, want),
			"the documented collision/limits/provenance policy must be visible at the verb; missing %q", want)
	}
}
