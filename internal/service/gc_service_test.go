package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gitstore "github.com/hyper-swe/mgit-dev/internal/store/git"
)

// --- GCService Tests ---
// Refs: FR-8.4, FR-13.2, MGIT-4.2.11

func setupGCEnv(t *testing.T) (*testEnv, *GCService) {
	t.Helper()
	env := setupTestEnv(t)
	gcStore := gitstore.NewGCStore(env.repo)
	gcSvc := NewGCService(gcStore)
	return env, gcSvc
}

func TestGCService_Run_NoOpBelowThreshold(t *testing.T) {
	_, gcSvc := setupGCEnv(t)
	ctx := context.Background()

	// Fresh repo has very few loose objects, below default threshold.
	result, err := gcSvc.Run(ctx, GCRequest{})
	require.NoError(t, err)
	assert.Equal(t, "no-op", result.Status)
	assert.False(t, result.Packed)
	assert.Equal(t, result.LooseBefore, result.LooseAfter)
}

func TestGCService_Run_Aggressive(t *testing.T) {
	env, gcSvc := setupGCEnv(t)
	ctx := context.Background()

	// Create some commits to generate loose objects.
	for i := range 5 {
		_, err := env.commit.CreateCommit(ctx, CreateCommitRequest{
			TaskID: "MGIT-11.1", AgentID: "a", Message: string(rune('A' + i)),
		})
		require.NoError(t, err)
	}

	result, err := gcSvc.Run(ctx, GCRequest{Aggressive: true})
	require.NoError(t, err)
	assert.True(t, result.Packed)
	assert.True(t, result.Aggressive)
	assert.Equal(t, "packed", result.Status)
}

func TestGCService_Run_CustomThreshold(t *testing.T) {
	env, gcSvc := setupGCEnv(t)
	ctx := context.Background()

	// Create a few commits to generate loose objects.
	for i := range 3 {
		_, err := env.commit.CreateCommit(ctx, CreateCommitRequest{
			TaskID: "MGIT-11.2", AgentID: "a", Message: string(rune('A' + i)),
		})
		require.NoError(t, err)
	}

	// Use a very low threshold (1) so packing is triggered.
	result, err := gcSvc.Run(ctx, GCRequest{PackThreshold: 1})
	require.NoError(t, err)
	assert.True(t, result.Packed)
	assert.Equal(t, "packed", result.Status)
}

func TestGCService_Run_AutoTriggered(t *testing.T) {
	env, gcSvc := setupGCEnv(t)
	ctx := context.Background()

	// Create commits for loose objects.
	for i := range 3 {
		_, err := env.commit.CreateCommit(ctx, CreateCommitRequest{
			TaskID: "MGIT-11.3", AgentID: "a", Message: string(rune('A' + i)),
		})
		require.NoError(t, err)
	}

	result, err := gcSvc.Run(ctx, GCRequest{
		Aggressive:    true,
		AutoTriggered: true,
	})
	require.NoError(t, err)
	assert.True(t, result.AutoTriggered)
	assert.Equal(t, "packed (auto)", result.Status)
}

func TestGCService_Run_DefaultThreshold(t *testing.T) {
	_, gcSvc := setupGCEnv(t)
	ctx := context.Background()

	// PackThreshold of 0 should default to DefaultGCPackThreshold.
	result, err := gcSvc.Run(ctx, GCRequest{PackThreshold: 0})
	require.NoError(t, err)
	// Fresh repo won't exceed 1000 loose objects, so expect no-op.
	assert.Equal(t, "no-op", result.Status)
}

func TestGCService_Run_NegativeThreshold(t *testing.T) {
	_, gcSvc := setupGCEnv(t)
	ctx := context.Background()

	// Negative threshold should also default to DefaultGCPackThreshold.
	result, err := gcSvc.Run(ctx, GCRequest{PackThreshold: -10})
	require.NoError(t, err)
	assert.Equal(t, "no-op", result.Status)
}
