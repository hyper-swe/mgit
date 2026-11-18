package service

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The reason given to a rollback is recorded in the revert commit and in the
// audit trail. It must not travel into the squash message mgit generates, which
// is what leaves the store for the user's own git. Refs: MGIT-329

const rollbackReasonText = "customer-visible-reason-4471"

// rolledBackTask builds a task with two steps, a rollback carrying the reason,
// and one more step after it, so the squash still has a net change.
func rolledBackTask(t *testing.T, env *testEnv, taskID string) {
	t.Helper()
	stageAndCommit(t, env, taskID, "a.txt", "a\n")
	stageAndCommit(t, env, taskID, "b.txt", "b\n")
	_, err := env.rollbk.RollbackTask(context.Background(), RollbackRequest{TaskID: taskID, Reason: rollbackReasonText})
	require.NoError(t, err)
	stageAndCommit(t, env, taskID, "c.txt", "c\n")
}

func TestGeneratedSquashMessageNamesARollbackWithoutItsReason(t *testing.T) {
	env, _ := setupAuditEnv(t)
	ctx := context.Background()
	rolledBackTask(t, env, "MGIT-329.1")

	dry, err := env.squash.SquashTask(ctx, SquashRequest{TaskID: "MGIT-329.1", DryRun: true})
	require.NoError(t, err)
	assert.NotContains(t, dry.Message, rollbackReasonText, "the generated message must not carry the reason")
	assert.Contains(t, dry.Message, "rolled back step 2", "the rollback is named by its step")

	c, err := env.squash.SquashTask(ctx, SquashRequest{TaskID: "MGIT-329.1"})
	require.NoError(t, err)
	patch, err := env.squash.GitFormatPatch(ctx, c)
	require.NoError(t, err)
	assert.NotContains(t, patch, rollbackReasonText, "the git patch must not carry the reason")
	assert.Contains(t, patch, "rolled back step 2")
	for _, p := range c.SquashedFrom {
		assert.False(t, strings.Contains(p, rollbackReasonText), "provenance lines carry no reason: %q", p)
	}
}

func TestGitExportPreviewCarriesNoRollbackReason(t *testing.T) {
	env, _ := setupAuditEnv(t)
	rolledBackTask(t, env, "MGIT-329.2")

	prev, err := env.squash.PreviewGitPatch(context.Background(), SquashRequest{TaskID: "MGIT-329.2"})
	require.NoError(t, err)
	assert.NotContains(t, prev.Patch, rollbackReasonText)
	assert.Contains(t, prev.Patch, "rolled back step 2")
}

func TestASuppliedSquashMessageIsStillRecordedVerbatim(t *testing.T) {
	env, _ := setupAuditEnv(t)
	rolledBackTask(t, env, "MGIT-329.3")

	c, err := env.squash.SquashTask(context.Background(), SquashRequest{TaskID: "MGIT-329.3", Message: "my words only"})
	require.NoError(t, err)
	assert.Equal(t, "my words only", c.Message)
}

func TestTheRollbackReasonStaysInTheAuditTrailAndTheRevertCommit(t *testing.T) {
	env, audit := setupAuditEnv(t)
	rolledBackTask(t, env, "MGIT-329.4")

	entries, err := audit.GetAuditLog(AuditFilters{Operation: AuditRollback})
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Contains(t, entries[0].Details, rollbackReasonText, "the audit trail keeps the reason")
}
