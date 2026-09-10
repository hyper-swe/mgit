package packaging

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The board-drift report is wired to every push to main, reads the board
// as of its last update, pipes the exact log format boardcheck documents,
// and ends clean once the status is written. Refs: MGIT-178
func TestBoardDrift_IsWiredToPushesOnMain(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), ".github", "workflows", "board-drift.yml"))
	require.NoError(t, err)
	wf := string(raw)
	for _, want := range []string{
		"branches: [main]",
		"statuses: write",
		"fetch-depth: 0",
		"go build -o /tmp/boardcheck ./scripts/boardcheck",
		"git log -1 --format=%H -- .mtix/tasks.json",
		"%(trailers:key=Refs,valueonly,separator=%x2C)",
		"%(trailers:key=Stays-Open,valueonly,separator=%x2C)",
		"-f context=board-drift",
		"\n          exit 0",
	} {
		assert.Contains(t, wf, want)
	}
	assert.NotContains(t, wf, "pull_request", "a PR head is not main; the board is compared where merges land")
	assert.NotContains(t, wf, "-strict", "a report, not a gate: the ruling makes the close part of the ritual, not a merge blocker")
	assert.NotContains(t, wf, "\n          exit $code")
}
