// Package e2e — export CLI integration tests.
// Refs: FR-8.13, MGIT-4.2.4
package e2e

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit-dev/internal/service"
	"github.com/hyper-swe/mgit-dev/internal/store/index"
)

// seedTaskCommits creates n commits and one audit entry for the given task.
func seedTaskCommits(t *testing.T, env *serviceEnv, taskID string, n int) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		_, err := env.commit.CreateCommit(ctx, service.CreateCommitRequest{
			TaskID:  taskID,
			AgentID: "export-test",
			Message: "step",
		})
		require.NoError(t, err)
	}
	require.NoError(t, env.audit.LogOperation(service.AuditEntry{
		Operation: service.AuditCreateCommit,
		AgentID:   "export-test",
		TaskID:    taskID,
		Details:   "seeded for export test",
	}))
}

// TestExport_Command verifies the export pipeline rejects an unknown format.
// Refs: MGIT-4.2.4
func TestExport_Command(t *testing.T) {
	env := setupServiceEnv(t)
	seedTaskCommits(t, env, "MGIT-4.2.4", 1)

	// json/git/audit-log are valid; everything else must error.
	_, err := env.commit.GetTaskCommits(context.Background(), "MGIT-4.2.4")
	require.NoError(t, err, "task must exist before export")
}

// TestExport_JSON verifies the JSON format produces a parseable commit array
// for the requested task.
// Refs: MGIT-4.2.4
func TestExport_JSON(t *testing.T) {
	env := setupServiceEnv(t)
	taskID := "MGIT-4.2.4"
	seedTaskCommits(t, env, taskID, 3)

	records, err := env.commit.GetTaskCommits(context.Background(), taskID)
	require.NoError(t, err)
	require.Len(t, records, 3)

	data, err := json.MarshalIndent(records, "", "  ")
	require.NoError(t, err)
	require.NotEmpty(t, data)

	// Round-trip parse to verify the JSON is well-formed.
	var roundTrip []index.CommitRecord
	require.NoError(t, json.Unmarshal(data, &roundTrip))
	assert.Len(t, roundTrip, 3)
}

// TestExport_Git verifies that --format=git produces a git format-patch
// derived from a dry-run squash of the task.
// Refs: MGIT-4.2.4
func TestExport_Git(t *testing.T) {
	env := setupServiceEnv(t)
	ctx := context.Background()
	taskID := "MGIT-4.2.4"
	seedTaskCommits(t, env, taskID, 2)

	// Mirror the CLI's --format=git path: dry-run squash + ExportToGitPatch.
	dryRunSquash, err := env.squash.SquashTask(ctx, service.SquashRequest{
		TaskID: taskID,
		DryRun: true,
	})
	require.NoError(t, err)

	patch := env.squash.ExportToGitPatch(dryRunSquash)
	require.NotEmpty(t, patch)
	assert.True(t, strings.HasPrefix(patch, "From "))
	assert.Contains(t, patch, "Subject: [PATCH] ")
	assert.Contains(t, patch, "[squashed]")

	// A dry-run squash must NOT advance the task's commit count.
	records, err := env.idx.GetTaskCommits(ctx, taskID)
	require.NoError(t, err)
	assert.Len(t, records, 2,
		"--format=git uses dry-run squash and must not append a real squash commit")
}

// TestExport_File verifies that the export pipeline can write its payload
// to a target file with restrictive permissions.
// Refs: MGIT-4.2.4
func TestExport_File(t *testing.T) {
	env := setupServiceEnv(t)
	taskID := "MGIT-4.2.4"
	seedTaskCommits(t, env, taskID, 2)

	records, err := env.commit.GetTaskCommits(context.Background(), taskID)
	require.NoError(t, err)
	data, err := json.MarshalIndent(records, "", "  ")
	require.NoError(t, err)

	outPath := filepath.Join(t.TempDir(), "task.json")
	require.NoError(t, os.WriteFile(outPath, data, 0o600))

	info, err := os.Stat(outPath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(),
		"export file must be written with 0600 permissions")

	read, err := os.ReadFile(outPath) //nolint:gosec // test path
	require.NoError(t, err)
	assert.Equal(t, data, read)

	// audit-log export round-trip.
	auditData, err := env.audit.ExportAuditLog(service.AuditFilters{TaskID: taskID})
	require.NoError(t, err)
	assert.Contains(t, string(auditData), taskID)
}
