package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
	gitstore "github.com/hyper-swe/mgit/internal/store/git"
)

// `mgit commit -a` is the instructed way to commit (CLAUDE.md, MGIT-77), which
// is exactly why it is the path that swept 21 MB and 40 MB build binaries into
// task branches. These tests pin the size tripwire on the service that every
// surface (CLI, MCP, REST) commits through. Refs: FR-2, MGIT-131

// writeBytes writes a file of exactly n bytes under the test repo root.
func writeBytes(t *testing.T, root, rel string, n int64) {
	t.Helper()
	abs := filepath.Join(root, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(abs), 0o750))
	require.NoError(t, os.WriteFile(abs, make([]byte, n), 0o600))
}

func TestCommitService_CreateCommit_StageAllOversizedFile_Refused(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()
	svc := env.commit.WithStagedFileLimit(1 << 20)

	require.NoError(t, os.WriteFile(filepath.Join(env.repo.Root(), "fix.go"),
		[]byte("package fix\n"), 0o600))
	writeBytes(t, env.repo.Root(), "build/mgit", 4<<20)

	before, err := env.cs.ListCommits(ctx)
	require.NoError(t, err)

	_, err = svc.CreateCommit(ctx, CreateCommitRequest{
		TaskID:   "MGIT-131",
		AgentID:  "agent-01",
		Message:  "step",
		StageAll: true,
	})

	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrFileTooLarge)
	assert.Contains(t, err.Error(), "build/mgit is 4.0 MB (limit 1.0 MB)")
	assert.Contains(t, err.Error(), "--allow-large", "the refusal must name its own override")

	after, err := env.cs.ListCommits(ctx)
	require.NoError(t, err)
	assert.Len(t, after, len(before), "a refused commit must not advance the branch")
}

func TestCommitService_CreateCommit_StageAllOversizedFileAllowLarge_Succeeds(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()
	svc := env.commit.WithStagedFileLimit(1 << 20)
	writeBytes(t, env.repo.Root(), "big.bin", 4<<20)

	c, err := svc.CreateCommit(ctx, CreateCommitRequest{
		TaskID:     "MGIT-131",
		AgentID:    "agent-01",
		Message:    "a legitimately large file, committed deliberately",
		StageAll:   true,
		AllowLarge: true,
	})

	require.NoError(t, err)
	require.NotEmpty(t, c.CommitID)
	data, err := env.cs.GetFileFromCommit(ctx, c.CommitID, "big.bin")
	require.NoError(t, err)
	assert.Len(t, data, 4<<20, "the escape hatch must actually record the file")
}

func TestCommitService_CreateCommit_StageAllUnderLimit_Unaffected(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()
	svc := env.commit.WithStagedFileLimit(1 << 20)
	writeBytes(t, env.repo.Root(), "just-under.bin", (1<<20)-1)

	c, err := svc.CreateCommit(ctx, CreateCommitRequest{
		TaskID:   "MGIT-131",
		AgentID:  "agent-01",
		Message:  "ordinary work",
		StageAll: true,
	})

	require.NoError(t, err)
	data, err := env.cs.GetFileFromCommit(ctx, c.CommitID, "just-under.bin")
	require.NoError(t, err)
	assert.Len(t, data, (1<<20)-1)
}

func TestCommitService_NewCommitService_DefaultLimit_GuardIsOn(t *testing.T) {
	// A surface that never calls WithStagedFileLimit (a REST/MCP server built
	// without a readable config) must still inherit the guard — the default is
	// armed, not off. Refs: MGIT-131
	env := setupTestEnv(t)
	ctx := context.Background()
	writeBytes(t, env.repo.Root(), "artifact.bin", gitstore.DefaultMaxStagedFileBytes+1)

	_, err := env.commit.CreateCommit(ctx, CreateCommitRequest{
		TaskID:   "MGIT-131",
		AgentID:  "agent-01",
		Message:  "step",
		StageAll: true,
	})

	require.ErrorIs(t, err, model.ErrFileTooLarge)
}

func TestConfig_StagedFileLimitBytes_ConfiguredValues_ResolveAsSpecified(t *testing.T) {
	tests := []struct {
		name string
		mb   int
		want int64
	}{
		{name: "default_five_megabytes", mb: 5, want: 5 << 20},
		{name: "raised_by_operator", mb: 64, want: 64 << 20},
		{name: "zero_disables_the_guard", mb: 0, want: 0},
		{name: "negative_disables_the_guard", mb: -1, want: 0},
		// Hostile config content must clamp, never wrap around into a value
		// that silently disarms a guard the operator believes they widened.
		{name: "absurd_value_clamps", mb: 1 << 60, want: 1<<63 - 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.Limits.MaxStagedFileMB = tt.mb
			assert.Equal(t, tt.want, cfg.StagedFileLimitBytes())
		})
	}
}

func TestDefaultConfig_StagedFileLimit_MatchesStoreDefault(t *testing.T) {
	// The documented default and the store's compiled-in default must be the
	// same number, or the config file lies about what is enforced.
	assert.Equal(t, gitstore.DefaultMaxStagedFileBytes,
		DefaultConfig().StagedFileLimitBytes())
}
