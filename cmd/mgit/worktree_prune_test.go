package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestWorktreePrune_RemovesStaleRegistryEntries is the cmd-level proof for
// `mgit worktree prune`, which previously had only service-layer coverage
// (service_operations_test.go) and no e2e or cmd-level test at all — the
// registered subcommand itself was unverified. A worktree becomes "stale"
// when its directory disappears without going through `worktree remove`
// (e.g. `rm -rf` by hand); prune is what reconciles the registry afterward.
// Refs: MGIT-61.13
func TestWorktreePrune_RemovesStaleRegistryEntries(t *testing.T) {
	repo := t.TempDir()
	t.Chdir(repo)
	require.NoError(t, runCLI(t, "init"))
	require.NoError(t, runCLI(t, "worktree", "add", "wt", "--task-id", "PRUNE-1"))

	// Delete the worktree directory directly, bypassing `worktree remove`, so
	// its registry entry goes stale.
	require.NoError(t, os.RemoveAll(filepath.Join(repo, "wt")))

	out, err := runCLI2(t, "worktree", "prune", "--dry-run")
	require.NoError(t, err)
	assert.Contains(t, out, "Would remove:")
	assert.Contains(t, out, "wt")

	out, err = runCLI2(t, "worktree", "prune")
	require.NoError(t, err)
	assert.Contains(t, out, "Removed:")
	assert.Contains(t, out, "wt")

	out, err = runCLI2(t, "worktree", "prune")
	require.NoError(t, err)
	assert.Contains(t, out, "No stale worktrees", "the second prune has nothing left to do")
}

// runCLI2 is runCLI (guest_checkpoint_test.go) but also returns the
// captured output; runCLI only logs it on failure, and prune's own output
// (not an error) is exactly what these assertions need.
func runCLI2(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := rootCmd()
	root.SetArgs(args)
	// cobra writes to os.Stdout by default when no writer is set; the prune
	// RunE itself writes via fmt.Fprintf(os.Stdout, ...) rather than
	// cmd.OutOrStdout(), so redirect the real os.Stdout for this call.
	r, w, err := os.Pipe()
	require.NoError(t, err)
	old := os.Stdout
	os.Stdout = w
	runErr := root.Execute()
	os.Stdout = old
	require.NoError(t, w.Close())
	buf := make([]byte, 64*1024)
	n, _ := r.Read(buf)
	return string(buf[:n]), runErr
}
