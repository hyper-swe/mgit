package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestWorktreeList_MarksMissingPathsPrunableAndSaysHowToClear: a registry row
// whose directory is gone reads as a live binding unless the listing says
// otherwise, so nothing tells a reader to prune and a task that shows as
// bound may refuse `mgit work`. The marker is git's word, the hint names the
// verb, --json carries the fact as a field, and prune --dry-run names exactly
// the marked rows. Refs: MGIT-194, FR-16
func TestWorktreeList_MarksMissingPathsPrunableAndSaysHowToClear(t *testing.T) {
	repo := t.TempDir()
	t.Chdir(repo)
	require.NoError(t, runCLI(t, "init"))
	require.NoError(t, runCLI(t, "worktree", "add", "keep", "--task-id", "LIST-1"))
	require.NoError(t, runCLI(t, "worktree", "add", "gone", "--task-id", "LIST-2"))
	require.NoError(t, os.RemoveAll(filepath.Join(repo, "gone")))

	out, err := runCLI2(t, "worktree", "list")
	require.NoError(t, err)
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		switch {
		case strings.Contains(line, "LIST-2"):
			assert.Contains(t, line, "prunable", "the missing row carries the marker: %q", line)
		case strings.Contains(line, "LIST-1"):
			assert.NotContains(t, line, "prunable", "the present row does not: %q", line)
		}
	}
	assert.Contains(t, out, "1 prunable")
	assert.Contains(t, out, "mgit worktree prune", "the listing says how to clear it")

	out, err = runCLI2(t, "worktree", "list", "--json")
	require.NoError(t, err)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &rows), "--json is one JSON document: %q", out)
	got := map[string]bool{}
	for _, r := range rows {
		p, _ := r["prunable"].(bool)
		got[r["task_id"].(string)] = p
	}
	assert.Equal(t, map[string]bool{"LIST-1": false, "LIST-2": true}, got)

	out, err = runCLI2(t, "worktree", "list", "--porcelain")
	require.NoError(t, err)
	assert.Regexp(t, `LIST-2 prunable`, out)
	assert.NotRegexp(t, `LIST-1 prunable`, out)

	out, err = runCLI2(t, "worktree", "prune", "--dry-run")
	require.NoError(t, err)
	assert.Contains(t, out, "Would remove:")
	assert.Contains(t, out, "gone")
	assert.NotContains(t, out, "keep", "prune --dry-run names exactly the prunable rows")
}
