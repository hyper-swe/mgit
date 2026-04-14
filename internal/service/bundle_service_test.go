package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- BundleService Tests ---
// Refs: FR-12.4, FR-12.5, MGIT-4.2.12

func TestBundleService_Export_SingleTask(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()
	clock := fixedClock()

	// Create commits to export.
	_, err := env.commit.CreateCommit(ctx, CreateCommitRequest{
		TaskID: "MGIT-12.1", AgentID: "a", Message: "first",
	})
	require.NoError(t, err)
	_, err = env.commit.CreateCommit(ctx, CreateCommitRequest{
		TaskID: "MGIT-12.1", AgentID: "a", Message: "second",
	})
	require.NoError(t, err)

	bundleSvc := NewBundleService(env.idx, clock)
	data, err := bundleSvc.Export(ctx, []string{"MGIT-12.1"})
	require.NoError(t, err)

	var bundle Bundle
	require.NoError(t, json.Unmarshal(data, &bundle))
	assert.Equal(t, BundleVersion, bundle.Version)
	assert.Equal(t, BundleFormat, bundle.Format)
	assert.Len(t, bundle.TaskCommits, 2)
	assert.Equal(t, 2, bundle.Manifest.CommitCount)
	assert.NotEmpty(t, bundle.Manifest.ChecksumSHA256)
}

func TestBundleService_Export_EmptyTaskList(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()
	clock := fixedClock()

	bundleSvc := NewBundleService(env.idx, clock)
	data, err := bundleSvc.Export(ctx, []string{})
	require.NoError(t, err)

	var bundle Bundle
	require.NoError(t, json.Unmarshal(data, &bundle))
	assert.Empty(t, bundle.TaskCommits)
	assert.Equal(t, 0, bundle.Manifest.CommitCount)
}

func TestBundleService_ImportMerge_RoundTrip(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()
	clock := fixedClock()

	// Create commits, export, then import into same index (merge mode skips dupes).
	_, err := env.commit.CreateCommit(ctx, CreateCommitRequest{
		TaskID: "MGIT-12.2", AgentID: "a", Message: "commit",
	})
	require.NoError(t, err)

	bundleSvc := NewBundleService(env.idx, clock)
	data, err := bundleSvc.Export(ctx, []string{"MGIT-12.2"})
	require.NoError(t, err)

	// Importing same data should skip all records (already present).
	result, err := bundleSvc.Import(ctx, data, ImportMerge)
	require.NoError(t, err)
	assert.Equal(t, ImportMerge, result.Mode)
	assert.Equal(t, "imported", result.Status)
	assert.Equal(t, 1, result.Total)
	// Records already exist, so all should be skipped.
	assert.Equal(t, 1, result.Skipped)
	assert.Equal(t, 0, result.Imported)
}

func TestBundleService_ImportReplace_ExistingTask(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()
	clock := fixedClock()

	_, err := env.commit.CreateCommit(ctx, CreateCommitRequest{
		TaskID: "MGIT-12.3", AgentID: "a", Message: "commit",
	})
	require.NoError(t, err)

	bundleSvc := NewBundleService(env.idx, clock)
	data, err := bundleSvc.Export(ctx, []string{"MGIT-12.3"})
	require.NoError(t, err)

	// Replace mode should refuse because task already has commits.
	_, err = bundleSvc.Import(ctx, data, ImportReplace)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "already has")
}

func TestBundleService_Import_InvalidJSON(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()
	clock := fixedClock()

	bundleSvc := NewBundleService(env.idx, clock)
	_, err := bundleSvc.Import(ctx, []byte("not json"), ImportMerge)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "parse")
}

func TestBundleService_Import_WrongFormat(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()
	clock := fixedClock()

	bundleSvc := NewBundleService(env.idx, clock)
	data, _ := json.Marshal(Bundle{
		Version: BundleVersion,
		Format:  "wrong-format",
	})
	_, err := bundleSvc.Import(ctx, data, ImportMerge)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not an mgit bundle")
}

func TestBundleService_Import_WrongVersion(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()
	clock := fixedClock()

	bundleSvc := NewBundleService(env.idx, clock)
	data, _ := json.Marshal(Bundle{
		Version: "99",
		Format:  BundleFormat,
	})
	_, err := bundleSvc.Import(ctx, data, ImportMerge)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported version")
}

func TestBundleService_Import_ChecksumMismatch(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()
	clock := fixedClock()

	_, err := env.commit.CreateCommit(ctx, CreateCommitRequest{
		TaskID: "MGIT-12.4", AgentID: "a", Message: "commit",
	})
	require.NoError(t, err)

	bundleSvc := NewBundleService(env.idx, clock)
	data, err := bundleSvc.Export(ctx, []string{"MGIT-12.4"})
	require.NoError(t, err)

	// Tamper with the checksum.
	var bundle Bundle
	require.NoError(t, json.Unmarshal(data, &bundle))
	bundle.Manifest.ChecksumSHA256 = "deadbeef"
	tampered, _ := json.Marshal(bundle)

	_, err = bundleSvc.Import(ctx, tampered, ImportMerge)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "checksum mismatch")
}

func TestBundleService_Import_CommitCountMismatch(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()
	clock := fixedClock()

	_, err := env.commit.CreateCommit(ctx, CreateCommitRequest{
		TaskID: "MGIT-12.5", AgentID: "a", Message: "commit",
	})
	require.NoError(t, err)

	bundleSvc := NewBundleService(env.idx, clock)
	data, err := bundleSvc.Export(ctx, []string{"MGIT-12.5"})
	require.NoError(t, err)

	// Tamper with commit count but keep the checksum valid.
	var bundle Bundle
	require.NoError(t, json.Unmarshal(data, &bundle))
	bundle.Manifest.CommitCount = 999
	tampered, _ := json.Marshal(bundle)

	_, err = bundleSvc.Import(ctx, tampered, ImportMerge)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "count mismatch")
}

func TestBundleService_Import_UnknownMode(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()
	clock := fixedClock()

	bundleSvc := NewBundleService(env.idx, clock)
	_, err := bundleSvc.Import(ctx, []byte("{}"), ImportMode("unknown"))
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unknown mode")
}

func TestBundleService_Import_DefaultMode(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()
	clock := fixedClock()

	// Export an empty bundle and import with empty mode (defaults to merge).
	bundleSvc := NewBundleService(env.idx, clock)
	data, err := bundleSvc.Export(ctx, []string{})
	require.NoError(t, err)

	result, err := bundleSvc.Import(ctx, data, "")
	require.NoError(t, err)
	assert.Equal(t, ImportMerge, result.Mode)
}

func TestBundleService_NewBundleService_NilClock(t *testing.T) {
	env := setupTestEnv(t)
	// When clock is nil, NewBundleService should provide a default.
	svc := NewBundleService(env.idx, nil)
	assert.NotNil(t, svc)
	assert.NotNil(t, svc.clock)
}

func TestBundleService_isDuplicateInsert(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil_error", nil, false},
		{"unique_constraint", assert.AnError, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isDuplicateInsert(tt.err)
			assert.Equal(t, tt.want, got)
		})
	}
}
