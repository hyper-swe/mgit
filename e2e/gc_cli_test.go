// Package e2e — gc CLI integration tests.
// Refs: FR-8.4, FR-13.2, MGIT-4.2.11
package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit-dev/internal/service"
	gitstore "github.com/hyper-swe/mgit-dev/internal/store/git"
)

// seedLooseObjects creates n commits to populate the loose object store.
// Each commit stages a unique file so go-git writes new blob/tree/commit
// objects on every iteration.
func seedLooseObjects(t *testing.T, env *serviceEnv, n int) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		path := fmt.Sprintf("gc/file-%03d.txt", i)
		full := filepath.Join(env.repo.Root(), path)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o750))
		require.NoError(t, os.WriteFile(full, []byte(fmt.Sprintf("content %d\n", i)), 0o600))
		require.NoError(t, env.worktree.Add(ctx, path))
		_, err := env.commit.CreateCommit(ctx, service.CreateCommitRequest{
			TaskID:  "MGIT-4.2.11",
			AgentID: "gc-test",
			Message: "seed",
		})
		require.NoError(t, err)
	}
}

// TestGC_LooseObjectsPacked_Success exceeds the threshold and verifies
// that loose objects get packed and stats are reported.
// Refs: FR-8.4, MGIT-4.2.11
func TestGC_LooseObjectsPacked_Success(t *testing.T) {
	env := setupServiceEnv(t)
	ctx := context.Background()

	seedLooseObjects(t, env, 5)
	gcStore := gitstore.NewGCStore(env.repo)
	looseBefore, err := gcStore.LooseObjectCount(ctx)
	require.NoError(t, err)
	require.Greater(t, looseBefore, 0, "must have loose objects before gc")

	// Use a low threshold so the test does not need 1000 commits.
	result, err := env.gc.Run(ctx, service.GCRequest{PackThreshold: 1})
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.True(t, result.Packed, "gc must pack when loose count exceeds threshold")
	assert.Equal(t, "packed", result.Status)
	assert.Equal(t, looseBefore, result.LooseBefore)
	assert.LessOrEqual(t, result.LooseAfter, looseBefore,
		"loose object count must not increase after packing")
}

// TestGC_AggressiveFlag_FullRepack verifies --aggressive triggers a pack
// regardless of the threshold.
// Refs: FR-8.4, MGIT-4.2.11
func TestGC_AggressiveFlag_FullRepack(t *testing.T) {
	env := setupServiceEnv(t)
	ctx := context.Background()

	seedLooseObjects(t, env, 3)

	result, err := env.gc.Run(ctx, service.GCRequest{
		Aggressive:    true,
		PackThreshold: 99999, // would normally suppress packing
	})
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.True(t, result.Aggressive)
	assert.True(t, result.Packed, "--aggressive must pack even when below threshold")
	assert.Equal(t, "packed", result.Status)
}

// TestGC_BelowThreshold_NoOp verifies that gc is a no-op when there are
// fewer loose objects than the threshold and --aggressive is not set.
// Refs: FR-8.4, MGIT-4.2.11
func TestGC_BelowThreshold_NoOp(t *testing.T) {
	env := setupServiceEnv(t)
	ctx := context.Background()

	seedLooseObjects(t, env, 2)

	result, err := env.gc.Run(ctx, service.GCRequest{PackThreshold: 99999})
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.False(t, result.Packed, "gc must not pack below threshold")
	assert.Equal(t, "no-op", result.Status)
	assert.Equal(t, result.LooseBefore, result.LooseAfter,
		"no-op gc must not change loose object count")
}

// TestGC_AutoTrigger_OnCommit verifies that an auto-triggered gc run is
// labeled as such in the result and runs the same logic as a manual run.
// Auto-trigger orchestration (when gc.auto=true) is the caller's job; this
// test confirms the GCRequest.AutoTriggered field flows through.
// Refs: FR-8.4, MGIT-4.2.11
func TestGC_AutoTrigger_OnCommit(t *testing.T) {
	env := setupServiceEnv(t)
	ctx := context.Background()

	seedLooseObjects(t, env, 3)

	result, err := env.gc.Run(ctx, service.GCRequest{
		PackThreshold: 1,
		AutoTriggered: true,
	})
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.True(t, result.AutoTriggered, "auto-triggered flag must be propagated")
	assert.True(t, result.Packed)
	assert.Equal(t, "packed (auto)", result.Status)
}
