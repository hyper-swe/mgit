package service

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupAuditEnv extends the standard test env with an AuditService wired into
// the commit, squash, and rollback services. Verifies MGIT-20: commit/squash/
// rollback operations append entries surfaced by the audit log.
func setupAuditEnv(t *testing.T) (*testEnv, *AuditService) {
	t.Helper()
	env := setupTestEnv(t)
	clock := fixedClock()
	logPath := filepath.Join(t.TempDir(), "audit.log")
	audit := NewAuditService(logPath, clock)

	env.commit.WithAudit(audit)
	env.squash.WithAudit(audit)
	env.rollbk.WithAudit(audit)
	return env, audit
}

func TestCommitService_CreateCommit_AppendsAuditEntry(t *testing.T) {
	env, audit := setupAuditEnv(t)
	ctx := context.Background()

	c, err := env.commit.CreateCommit(ctx, CreateCommitRequest{
		TaskID:  "MGIT-20.1",
		AgentID: "agent-audit",
		Message: "wire audit",
	})
	require.NoError(t, err)

	entries, err := audit.GetAuditLog(AuditFilters{})
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, AuditCreateCommit, entries[0].Operation)
	assert.Equal(t, "MGIT-20.1", entries[0].TaskID)
	assert.Equal(t, "agent-audit", entries[0].AgentID)
	assert.Equal(t, c.CommitID, entries[0].CommitID)
	assert.NotEmpty(t, entries[0].Timestamp)
}

func TestCommitService_CreateCommit_NoAudit_StillSucceeds(t *testing.T) {
	env := setupTestEnv(t) // no audit wired
	ctx := context.Background()

	_, err := env.commit.CreateCommit(ctx, CreateCommitRequest{
		TaskID:  "MGIT-20.2",
		AgentID: "agent-audit",
		Message: "no audit",
	})
	require.NoError(t, err)
}

func TestSquashService_SquashTask_AppendsAuditEntry(t *testing.T) {
	env, audit := setupAuditEnv(t)
	ctx := context.Background()

	for i := range 2 {
		_, err := env.commit.CreateCommit(ctx, CreateCommitRequest{
			TaskID:  "MGIT-20.3",
			AgentID: "agent-audit",
			Message: "c" + string(rune('A'+i)),
		})
		require.NoError(t, err)
	}

	sq, err := env.squash.SquashTask(ctx, SquashRequest{TaskID: "MGIT-20.3"})
	require.NoError(t, err)

	entries, err := audit.GetAuditLog(AuditFilters{Operation: AuditSquash})
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, AuditSquash, entries[0].Operation)
	assert.Equal(t, "MGIT-20.3", entries[0].TaskID)
	assert.Equal(t, sq.CommitID, entries[0].CommitID)
}

func TestSquashService_SquashTask_DryRun_NoAuditEntry(t *testing.T) {
	env, audit := setupAuditEnv(t)
	ctx := context.Background()

	_, err := env.commit.CreateCommit(ctx, CreateCommitRequest{
		TaskID: "MGIT-20.4", AgentID: "agent-audit", Message: "c",
	})
	require.NoError(t, err)

	_, err = env.squash.SquashTask(ctx, SquashRequest{TaskID: "MGIT-20.4", DryRun: true})
	require.NoError(t, err)

	entries, err := audit.GetAuditLog(AuditFilters{Operation: AuditSquash})
	require.NoError(t, err)
	assert.Empty(t, entries, "dry run must not append an audit entry")
}

func TestRollbackService_RollbackTask_AppendsAuditEntry(t *testing.T) {
	env, audit := setupAuditEnv(t)
	ctx := context.Background()

	stageAndCommit(t, env, "MGIT-20.5", "audited.txt", "c\n")

	rb, err := env.rollbk.RollbackTask(ctx, RollbackRequest{TaskID: "MGIT-20.5"})
	require.NoError(t, err)

	entries, err := audit.GetAuditLog(AuditFilters{Operation: AuditRollback})
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, AuditRollback, entries[0].Operation)
	assert.Equal(t, "MGIT-20.5", entries[0].TaskID)
	assert.Equal(t, rb.CommitID, entries[0].CommitID)
}

func TestRollbackService_RollbackTask_DryRun_NoAuditEntry(t *testing.T) {
	env, audit := setupAuditEnv(t)
	ctx := context.Background()

	stageAndCommit(t, env, "MGIT-20.6", "audited2.txt", "c\n")

	_, err := env.rollbk.RollbackTask(ctx, RollbackRequest{TaskID: "MGIT-20.6", DryRun: true})
	require.NoError(t, err)

	entries, err := audit.GetAuditLog(AuditFilters{Operation: AuditRollback})
	require.NoError(t, err)
	assert.Empty(t, entries, "dry run must not append an audit entry")
}
