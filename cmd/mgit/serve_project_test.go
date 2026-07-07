package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gitstore "github.com/hyper-swe/mgit/internal/store/git"
)

// TestServe_ProjectFlag_Registered verifies `mgit serve` exposes --project so
// the Claude Desktop app can target a repo without a cwd dependency.
// Refs: MGIT-60
func TestServe_ProjectFlag_Registered(t *testing.T) {
	f := serveCmd().Flags().Lookup("project")
	require.NotNil(t, f, "serve must expose --project")
	assert.Empty(t, f.DefValue, "default is empty (cwd behavior preserved)")
}

// TestOpenAppAt_ResolvesGivenRepo verifies openAppAt opens the repo at an
// explicit path (and its subdirectories), independent of cwd. Refs: MGIT-60
func TestOpenAppAt_ResolvesGivenRepo(t *testing.T) {
	dir := t.TempDir()
	_, err := gitstore.Init(dir, testClock())
	require.NoError(t, err)

	app, err := openAppAt(dir)
	require.NoError(t, err)
	app.Close() // release the lifetime lock before re-opening the same repo

	// A subdirectory resolves to the same repo root.
	sub := filepath.Join(dir, "pkg", "inner")
	require.NoError(t, os.MkdirAll(sub, 0o750))
	app2, err := openAppAt(sub)
	require.NoError(t, err)
	app2.Close()
}

// TestOpenAppAt_NonRepo_Errors verifies a path with no .mgit fails with a
// clear not-a-repository error rather than silently serving nothing.
// Refs: MGIT-60
func TestOpenAppAt_NonRepo_Errors(t *testing.T) {
	_, err := openAppAt(t.TempDir())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not an mgit repository")
}

// TestOpenServeApp_ProjectOverridesCwd verifies openServeApp uses --project
// when set (regardless of cwd), and falls back to cwd when unset.
// Refs: MGIT-60
func TestOpenServeApp_ProjectOverridesCwd(t *testing.T) {
	project := t.TempDir()
	_, err := gitstore.Init(project, testClock())
	require.NoError(t, err)

	// cwd is a NON-repo temp dir; --project must still resolve the repo.
	t.Chdir(t.TempDir())
	app, err := openServeApp(project)
	require.NoError(t, err)
	app.Close()

	// Unset --project from a non-repo cwd fails (cwd fallback, no repo).
	_, err = openServeApp("")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not an mgit repository")
}
