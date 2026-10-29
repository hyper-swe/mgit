package packaging

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// A base that absorbed an untracked runtime path is repaired by excluding the
// path and running one explicit capture; the README says so, says what stays
// unchanged, and says which paths cannot be repaired this way. The behaviour
// is covered by TestE2E_Resync_IgnoredUntrackedCaptureOrders.
// Refs: MGIT-324

func TestDocs_ABaseThatAbsorbedAnUntrackedPathIsRepairedByExclusionAndOneCapture(t *testing.T) {
	readme := flatten(readRepoFile(t, "README.md"))
	for _, want := range []string{
		"excluding the path and running one explicit capture (`mgit work <path> --task-id <ID> --include-uncommitted`, which also creates a task worktree)",
		"Tasks created earlier keep the base they forked from",
		"A path that Git tracks and the working tree still holds can't be repaired this way",
	} {
		assert.Contains(t, readme, want, "README.md must say %q", want)
	}
}
