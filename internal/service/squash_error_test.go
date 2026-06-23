package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSquashService_SquashTask_BadRecord_Error: when a task has an index record
// pointing at a commit absent from the object store, squashing the task fails at
// the per-commit lookup (not a silent skip). Refs: FR-7
func TestSquashService_SquashTask_BadRecord_Error(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	require.NoError(t, env.idx.AddCommitToTask(ctx, "MGIT-5.9",
		"0123456789abcdef0123456789abcdef01234567", "deadbeef", "agent", 0))

	_, err := env.squash.SquashTask(ctx, SquashRequest{TaskID: "MGIT-5.9"})
	assert.Error(t, err, "squashing a task whose commit is missing must error")
}
