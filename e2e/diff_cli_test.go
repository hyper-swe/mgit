// Package e2e — diff CLI integration tests.
// Refs: FR-8.5, FR-11, MGIT-4.1.4
package e2e

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/service"
)

// TestDiff_Command verifies the diff service is wired into the env and
// can be invoked end-to-end against a real repository.
// Refs: MGIT-4.1.4
func TestDiff_Command(t *testing.T) {
	env := setupServiceEnv(t)
	require.NotNil(t, env.diff, "diff service must be initialized in env")

	// Smoke: an empty repo with no commits should return ErrTaskNotFound.
	_, err := env.diff.DiffTask(context.Background(), "MGIT-9.9.9")
	require.Error(t, err)
}

// TestDiff_BetweenCommits exercises DiffCommits across two real commits.
// Refs: FR-11, MGIT-4.1.4
func TestDiff_BetweenCommits(t *testing.T) {
	env := setupServiceEnv(t)
	ctx := context.Background()

	// Snapshot HEAD before any new commits.
	from, err := env.repo.Head()
	require.NoError(t, err)

	// Create two commits on the same task.
	_, err = env.commit.CreateCommit(ctx, service.CreateCommitRequest{
		TaskID:  "MGIT-4.1.4",
		AgentID: "diff-test",
		Message: "first change",
	})
	require.NoError(t, err)

	_, err = env.commit.CreateCommit(ctx, service.CreateCommitRequest{
		TaskID:  "MGIT-4.1.4",
		AgentID: "diff-test",
		Message: "second change",
	})
	require.NoError(t, err)

	to, err := env.repo.Head()
	require.NoError(t, err)
	require.NotEqual(t, from, to, "HEAD should advance after commits")

	diffs, err := env.diff.DiffCommits(ctx, from, to)
	require.NoError(t, err)
	assert.NotNil(t, diffs)

	// FormatUnified must produce non-empty output for non-empty diffs and
	// must produce empty output for nil diffs.
	out := env.diff.FormatUnified(diffs)
	if len(diffs) > 0 {
		assert.NotEmpty(t, out)
	}
	assert.Empty(t, env.diff.FormatUnified(nil))
}

// TestDiff_Task verifies the cumulative diff for a task.
// Refs: FR-11, MGIT-4.1.4
func TestDiff_Task(t *testing.T) {
	env := setupServiceEnv(t)
	ctx := context.Background()

	taskID := "MGIT-4.1.4"
	for i := 0; i < 3; i++ {
		_, err := env.commit.CreateCommit(ctx, service.CreateCommitRequest{
			TaskID:  taskID,
			AgentID: "diff-test",
			Message: "step",
		})
		require.NoError(t, err)
	}

	diffs, err := env.diff.DiffTask(ctx, taskID)
	require.NoError(t, err)
	assert.NotNil(t, diffs)

	// A nonexistent task must return ErrTaskNotFound.
	_, err = env.diff.DiffTask(ctx, "MGIT-0.0.0")
	require.Error(t, err)
}

// TestDiff_Statistics verifies FormatStat output for both empty and
// non-empty diff slices.
// Refs: FR-11, MGIT-4.1.4
func TestDiff_Statistics(t *testing.T) {
	env := setupServiceEnv(t)
	ctx := context.Background()

	// Empty diffs → "0 files changed".
	statEmpty := env.diff.FormatStat(nil)
	assert.Contains(t, statEmpty, "0 files changed")
	assert.Contains(t, statEmpty, "0 insertions(+)")
	assert.Contains(t, statEmpty, "0 deletions(-)")

	// Real commits → real stat output.
	from, err := env.repo.Head()
	require.NoError(t, err)

	_, err = env.commit.CreateCommit(ctx, service.CreateCommitRequest{
		TaskID:  "MGIT-4.1.4",
		AgentID: "diff-test",
		Message: "stat test",
	})
	require.NoError(t, err)

	to, err := env.repo.Head()
	require.NoError(t, err)

	diffs, err := env.diff.DiffCommits(ctx, from, to)
	require.NoError(t, err)

	stat := env.diff.FormatStat(diffs)
	assert.Contains(t, stat, "files changed")
	assert.Contains(t, stat, "insertions(+)")
	assert.Contains(t, stat, "deletions(-)")
}
