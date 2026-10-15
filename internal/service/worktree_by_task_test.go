package service

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// The CLI resolves the agent a worktree is bound to through the worktree
// service, not by reaching into the index store. Refs: MGIT-117
func TestWorktreeService_ByTask_ReturnsTheBoundWorktree(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()
	wtSvc := NewWorktreeService(env.idx, env.branch, env.wt, fixedClock())
	_, err := wtSvc.Add(ctx, model.WorktreeAddOptions{Path: filepath.Join(t.TempDir(), "wt"), TaskID: "MGIT-9.1", AgentID: "agent-7"})
	require.NoError(t, err)

	got, err := wtSvc.ByTask(ctx, "MGIT-9.1")
	require.NoError(t, err)
	assert.Equal(t, "agent-7", got.AgentID)
	assert.Equal(t, "MGIT-9.1", got.TaskID)
}

func TestWorktreeService_ByTask_AnUnboundTaskIsNotFound(t *testing.T) {
	env := setupTestEnv(t)
	wtSvc := NewWorktreeService(env.idx, env.branch, env.wt, fixedClock())

	_, err := wtSvc.ByTask(context.Background(), "MGIT-9.2")
	assert.ErrorIs(t, err, model.ErrWorktreeNotFound)
}
