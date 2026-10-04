package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// mgit's own housekeeping commits (a base resync, a task's fork base) carry
// no task and so no index entry; verify must not flag them, and must still
// flag any other commit that lacks one. Refs: MGIT-283, FR-12
func TestVerifyIndexIntegrity_SyncCommits_NotFlagged(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()
	writeProjectFile(t, env, "base.go", "package base\n")
	require.NoError(t, env.wt.Add(ctx, "base.go"))
	_, err := env.cs.CreateCommit(ctx, &model.Commit{AgentID: model.SyncAgentID, Message: "[mgit-sync] resync base"})
	require.NoError(t, err)

	issues, err := NewVerifyService(env.cs, env.idx).VerifyIndexIntegrity(ctx)
	require.NoError(t, err)
	assert.Empty(t, issues)
}

func TestVerifyIndexIntegrity_UnindexedNonSyncCommit_StillFlagged(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()
	writeProjectFile(t, env, "x.go", "package x\n")
	require.NoError(t, env.wt.Add(ctx, "x.go"))
	_, err := env.cs.CreateCommit(ctx, &model.Commit{AgentID: model.SyncAgentID, Message: "not a sync message"})
	require.NoError(t, err)

	issues, err := NewVerifyService(env.cs, env.idx).VerifyIndexIntegrity(ctx)
	require.NoError(t, err)
	assert.NotEmpty(t, issues, "only the sync author AND the sync message prefix exempt a commit")
}
