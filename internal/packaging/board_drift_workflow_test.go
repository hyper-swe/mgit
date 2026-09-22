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
// and ends clean once the status is written. The window starts at the
// board commit's parent so the board commit's own trailers are read.
// Refs: MGIT-178, MGIT-216
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
		// The window INCLUDES the commit that last touched the board: it is
		// the boundary, and its own trailers — the Refs it closes, the
		// Stays-Open that acknowledges a ticket the export could not close —
		// must be read, or a ticket named before the board commit leaves the
		// report by reset and the board commit can never say why (MGIT-216).
		"from=\"$since^\"",
		"git rev-parse --verify -q \"$from\"",
		"$from..HEAD",
		"--no-merges",
		"%(trailers:key=Refs,valueonly,separator=%x2C)",
		"%(trailers:key=Stays-Open,valueonly,separator=%x2C)",
		"-f context=board-drift",
		"grep '^status: '",
		"\n          exit 0",
	} {
		assert.Contains(t, wf, want)
	}
	assert.NotContains(t, wf, "cut -c1-140", "the tool bounds its own status line and names what it left out; the workflow no longer cuts names silently (MGIT-211)")
	assert.NotContains(t, wf, "pull_request", "a PR head is not main; the board is compared where merges land")
	assert.NotContains(t, wf, "-strict", "a report, not a gate: the ruling makes the close part of the ritual, not a merge blocker")
	assert.NotContains(t, wf, "\n          exit $code")
	assert.NotContains(t, wf, "\"$since..HEAD\"", "the old window excluded the board commit, so its own Stays-Open was never read and the drift it did not close vanished by reset (MGIT-216)")
}
