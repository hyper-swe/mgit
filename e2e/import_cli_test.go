// Package e2e — import CLI integration tests.
// Refs: FR-12.4, FR-12.5, MGIT-4.2.12
package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit-dev/internal/model"
	"github.com/hyper-swe/mgit-dev/internal/service"
)

// seedAndExport creates n commits for taskID and returns a valid bundle
// JSON payload exported by the bundle service.
func seedAndExport(t *testing.T, env *serviceEnv, taskID string, n int) []byte {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		_, err := env.commit.CreateCommit(ctx, service.CreateCommitRequest{
			TaskID:  taskID,
			AgentID: "import-test",
			Message: "seed",
		})
		require.NoError(t, err)
	}
	data, err := env.bundle.Export(ctx, []string{taskID})
	require.NoError(t, err)
	require.NotEmpty(t, data)
	return data
}

// TestImport_ValidBundle_Success exports from one repo and imports the
// resulting bundle into a second, fresh repo. The destination must hold
// the same task records after import.
// Refs: MGIT-4.2.12
func TestImport_ValidBundle_Success(t *testing.T) {
	src := setupServiceEnv(t)
	taskID := "MGIT-4.2.12"
	bundleData := seedAndExport(t, src, taskID, 3)

	dst := setupServiceEnv(t)
	result, err := dst.bundle.Import(context.Background(), bundleData, service.ImportMerge)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, 3, result.Imported)
	assert.Equal(t, 0, result.Skipped)
	assert.Equal(t, "imported", result.Status)

	got, err := dst.idx.GetTaskCommits(context.Background(), taskID)
	require.NoError(t, err)
	assert.Len(t, got, 3, "destination must hold the imported records")
}

// TestImport_CorruptedBundle_ReturnsError verifies that tampering with
// the bundle's checksum causes Import to abort with ErrVerificationFailed.
// Refs: FR-12.5, MGIT-4.2.12
func TestImport_CorruptedBundle_ReturnsError(t *testing.T) {
	src := setupServiceEnv(t)
	bundleData := seedAndExport(t, src, "MGIT-4.2.12", 2)

	// Decode, corrupt the checksum, re-encode.
	var b service.Bundle
	require.NoError(t, json.Unmarshal(bundleData, &b))
	b.Manifest.ChecksumSHA256 = "deadbeef" // intentionally wrong
	corrupted, err := json.Marshal(b)
	require.NoError(t, err)

	dst := setupServiceEnv(t)
	_, err = dst.bundle.Import(context.Background(), corrupted, service.ImportMerge)
	require.Error(t, err)
	assert.True(t, errors.Is(err, model.ErrVerificationFailed),
		"corrupted manifest must return ErrVerificationFailed, got %v", err)

	// A second corruption mode: garbage payload that fails JSON decode.
	_, err = dst.bundle.Import(context.Background(), []byte("{not json"), service.ImportMerge)
	require.Error(t, err)
	assert.True(t, errors.Is(err, model.ErrVerificationFailed),
		"unparseable bundle must return ErrVerificationFailed, got %v", err)
}

// TestImport_MergeMode_AddsToExisting verifies that --merge appends new
// records to a destination repo and silently skips duplicates.
// Refs: MGIT-4.2.12
func TestImport_MergeMode_AddsToExisting(t *testing.T) {
	ctx := context.Background()

	src := setupServiceEnv(t)
	bundleData := seedAndExport(t, src, "MGIT-4.2.12", 3)

	dst := setupServiceEnv(t)
	// Pre-seed the destination with one commit on a different task.
	_, err := dst.commit.CreateCommit(ctx, service.CreateCommitRequest{
		TaskID: "MGIT-4.2.99", AgentID: "import-test", Message: "pre-existing",
	})
	require.NoError(t, err)

	result, err := dst.bundle.Import(ctx, bundleData, service.ImportMerge)
	require.NoError(t, err)
	assert.Equal(t, 3, result.Imported)
	assert.Equal(t, 0, result.Skipped)

	// Existing task is untouched.
	preExisting, err := dst.idx.GetTaskCommits(ctx, "MGIT-4.2.99")
	require.NoError(t, err)
	assert.Len(t, preExisting, 1)

	// Imported task records are present.
	imported, err := dst.idx.GetTaskCommits(ctx, "MGIT-4.2.12")
	require.NoError(t, err)
	assert.Len(t, imported, 3)

	// A second merge of the same bundle is idempotent: every record is
	// detected as a duplicate and skipped.
	result2, err := dst.bundle.Import(ctx, bundleData, service.ImportMerge)
	require.NoError(t, err)
	assert.Equal(t, 0, result2.Imported)
	assert.Equal(t, 3, result2.Skipped)
}

// TestImport_ReplaceMode_OverwritesRepo verifies that replace mode
// succeeds against an empty destination and refuses against a non-empty
// one (because task_commits is append-only).
// Refs: MGIT-4.2.12
func TestImport_ReplaceMode_OverwritesRepo(t *testing.T) {
	ctx := context.Background()
	taskID := "MGIT-4.2.12"

	src := setupServiceEnv(t)
	bundleData := seedAndExport(t, src, taskID, 2)

	// Replace into a fresh repo: succeeds.
	freshDst := setupServiceEnv(t)
	result, err := freshDst.bundle.Import(ctx, bundleData, service.ImportReplace)
	require.NoError(t, err)
	assert.Equal(t, service.ImportReplace, result.Mode)
	assert.Equal(t, 2, result.Imported)

	got, err := freshDst.idx.GetTaskCommits(ctx, taskID)
	require.NoError(t, err)
	assert.Len(t, got, 2)

	// Replace into a destination that already holds the task: refused
	// because task_commits is append-only and cannot be deleted.
	dirtyDst := setupServiceEnv(t)
	_, err = dirtyDst.commit.CreateCommit(ctx, service.CreateCommitRequest{
		TaskID: taskID, AgentID: "import-test", Message: "pre-existing",
	})
	require.NoError(t, err)

	_, err = dirtyDst.bundle.Import(ctx, bundleData, service.ImportReplace)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "append-only")
}
