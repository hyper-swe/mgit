// Package e2e — mgit must work from any subdirectory of the repo (like git
// walking up to .git), not only from the repo root. Refs: MGIT-24
package e2e

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestE2E_RunsFromSubdirectory drives the real mgit binary from a nested
// subdirectory and asserts it resolves the repo's .mgit by walking up, rather
// than failing with "repository does not exist". Refs: MGIT-24
func TestE2E_RunsFromSubdirectory(t *testing.T) {
	bin := buildMgitBinary(t)
	repoDir := t.TempDir()
	gitCmd(t, repoDir, "init")

	out, err := runMgit(t, bin, repoDir, "init")
	require.NoError(t, err, "mgit init: %s", out)
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, "a.go"), []byte("package a\n"), 0o600))
	_, err = runMgit(t, bin, repoDir, "add", "a.go")
	require.NoError(t, err)
	_, err = runMgit(t, bin, repoDir, "commit", "--task-id", "MGIT-1", "-m", "a")
	require.NoError(t, err)

	// From a nested subdirectory, a read command must resolve the repo by
	// walking up to the .mgit store.
	deep := filepath.Join(repoDir, "sub", "deep")
	require.NoError(t, os.MkdirAll(deep, 0o750))
	out, err = runMgit(t, bin, deep, "log")
	require.NoError(t, err, "mgit log from a subdirectory must succeed: %s", out)
	assert.Contains(t, out, "MGIT-1", "log from a subdirectory must show the repo's history")
}
