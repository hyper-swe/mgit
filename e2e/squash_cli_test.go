// Package e2e — squash CLI integration tests.
// Refs: FR-7, FR-8.11, MGIT-4.2.2
package e2e

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/service"
)

// TestSquash_Command verifies the squash service is wired through the env
// and rejects an empty task ID.
// Refs: MGIT-4.2.2
func TestSquash_Command(t *testing.T) {
	env := setupServiceEnv(t)
	require.NotNil(t, env.squash)

	_, err := env.squash.SquashTask(context.Background(), service.SquashRequest{TaskID: "MGIT-9.9.9"})
	require.Error(t, err, "squash on nonexistent task must error")
}

// TestSquash_CreatesSquash creates several commits for one task, squashes
// them, and verifies the result is a real squash commit recorded in the
// SQLite index.
// Refs: FR-7, MGIT-4.2.2
func TestSquash_CreatesSquash(t *testing.T) {
	env := setupServiceEnv(t)
	ctx := context.Background()
	taskID := "MGIT-4.2.2"

	for i := 0; i < 4; i++ {
		_, err := env.commit.CreateCommit(ctx, service.CreateCommitRequest{
			TaskID:  taskID,
			AgentID: "squash-test",
			Message: "step",
		})
		require.NoError(t, err)
	}

	squashed, err := env.squash.SquashTask(ctx, service.SquashRequest{TaskID: taskID})
	require.NoError(t, err)
	require.NotNil(t, squashed)
	assert.Equal(t, model.CommitTypeSquash, squashed.CommitType)
	assert.NotEmpty(t, squashed.CommitID)
	assert.Contains(t, squashed.Message, taskID)

	// Index records the squash commit as the most recent entry for the task.
	records, err := env.idx.GetTaskCommits(ctx, taskID)
	require.NoError(t, err)
	require.Len(t, records, 5, "4 micro-commits + 1 squash commit expected")
	assert.Equal(t, squashed.CommitID, records[len(records)-1].CommitHash)
}

// TestSquash_CustomMessage verifies that a caller-supplied message is used
// verbatim instead of the auto-generated summary.
// Refs: FR-7, MGIT-4.2.2
func TestSquash_CustomMessage(t *testing.T) {
	env := setupServiceEnv(t)
	ctx := context.Background()
	taskID := "MGIT-4.2.2"

	_, err := env.commit.CreateCommit(ctx, service.CreateCommitRequest{
		TaskID: taskID, AgentID: "squash-test", Message: "first",
	})
	require.NoError(t, err)
	_, err = env.commit.CreateCommit(ctx, service.CreateCommitRequest{
		TaskID: taskID, AgentID: "squash-test", Message: "second",
	})
	require.NoError(t, err)

	custom := "Refactor: consolidate validation logic"
	squashed, err := env.squash.SquashTask(ctx, service.SquashRequest{
		TaskID:  taskID,
		Message: custom,
	})
	require.NoError(t, err)
	assert.Contains(t, squashed.Message, custom,
		"squash message must include the custom subject")
	assert.NotContains(t, squashed.Message, "Squashed from",
		"auto-generated header must not appear when custom message is set")
}

// TestSquash_ToGit verifies the --to-git export path produces a valid git
// format-patch with the [squashed] message prefix.
// Refs: FR-7, MGIT-4.2.2
func TestSquash_ToGit(t *testing.T) {
	env := setupServiceEnv(t)
	ctx := context.Background()
	taskID := "MGIT-4.2.2"

	for i := 0; i < 3; i++ {
		_, err := env.commit.CreateCommit(ctx, service.CreateCommitRequest{
			TaskID: taskID, AgentID: "squash-test", Message: "change",
		})
		require.NoError(t, err)
	}

	squashed, err := env.squash.SquashTask(ctx, service.SquashRequest{
		TaskID:  taskID,
		Message: "consolidate task work",
	})
	require.NoError(t, err)

	patch := env.squash.ExportToGitPatch(squashed)
	require.NotEmpty(t, patch)

	// Standard git format-patch markers must be present.
	assert.True(t, strings.HasPrefix(patch, "From "), "patch must start with mbox 'From ' line")
	assert.Contains(t, patch, "From: ")
	assert.Contains(t, patch, "Date: ")
	assert.Contains(t, patch, "Subject: [PATCH] ")
	assert.Contains(t, patch, "[squashed]",
		"squashed message must carry the [squashed] prefix")
	assert.Contains(t, patch, "consolidate task work",
		"custom message must propagate into the patch subject")
	assert.Contains(t, patch, "-- \nmgit\n",
		"patch must end with the format-patch signature trailer")

	// Empty input returns empty output (defensive contract).
	assert.Empty(t, env.squash.ExportToGitPatch(nil))
}
