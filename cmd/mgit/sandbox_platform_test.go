package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// TestSandboxPlatformRefusal_ByPlatform checks which host platforms the sandbox
// commands accept. The platform names are written out here, not read from the
// code under test.
func TestSandboxPlatformRefusal_ByPlatform(t *testing.T) {
	tests := []struct {
		goos    string
		refused bool
	}{
		{"linux", false},
		{"darwin", false},
		{"windows", true},
	}
	for _, tt := range tests {
		t.Run(tt.goos, func(t *testing.T) {
			err := sandboxPlatformRefusal(tt.goos)
			if !tt.refused {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.ErrorIs(t, err, model.ErrSandboxBackendUnavailable)
			assert.Contains(t, err.Error(), "not available on Windows yet")
			assert.Contains(t, err.Error(), "no sandbox backend for Windows")
			assert.Contains(t, err.Error(), "docs/adr/017-windows-wsl2-sandbox.md")
		})
	}
}

// TestSandboxConnectFor_OnWindows_ReportsUnavailableBeforeAnythingElse checks
// that the refusal comes first: not "not an mgit repository" and not
// "mgit-sandboxd binary not found".
func TestSandboxConnectFor_OnWindows_ReportsUnavailableBeforeAnythingElse(t *testing.T) {
	old := sandboxHostOS
	sandboxHostOS = "windows"
	t.Cleanup(func() { sandboxHostOS = old })

	// A directory that is not a repository: any later step would fail differently.
	_, err := sandboxConnectFor(context.Background(), t.TempDir())
	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrSandboxBackendUnavailable)
	assert.Contains(t, err.Error(), "not available on Windows yet")
}
