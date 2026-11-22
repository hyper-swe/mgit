package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// TestSandboxBaseFrom_OnWindows_RefusesBeforeAnyPullOrWrite pins that composing
// a guest base on a Windows host says the sandbox is unavailable there, as
// `sandbox launch` does, before it pulls an image or writes a base cache. The
// base is only usable by the sandbox, and a compose on Windows died on a
// symlink privilege error after the download.
func TestSandboxBaseFrom_OnWindows_RefusesBeforeAnyPullOrWrite(t *testing.T) {
	old := sandboxHostOS
	sandboxHostOS = "windows"
	t.Cleanup(func() { sandboxHostOS = old })

	cache := t.TempDir()
	t.Setenv("MGIT_BASE_CACHE", cache)
	repo := t.TempDir()
	t.Chdir(repo)
	require.NoError(t, os.WriteFile(filepath.Join(repo, "seed.txt"), []byte("seed\n"), 0o600))
	require.NoError(t, runCLI(t, "init"))

	// A reference that no registry serves: if the command went on to pull, the
	// error would name the registry or the network instead of the platform.
	out, err := runCLIOut(t, "sandbox", "base", "from", "registry.invalid/none:0")
	require.Error(t, err, out)
	assert.ErrorIs(t, err, model.ErrSandboxBackendUnavailable)
	assert.Contains(t, err.Error(), "not available on Windows yet")
	assert.NotContains(t, err.Error(), "registry.invalid")

	entries, readErr := os.ReadDir(cache)
	require.NoError(t, readErr)
	assert.Empty(t, entries, "the refusal came after a write into the base cache")
}
