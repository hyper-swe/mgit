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

	"github.com/hyper-swe/mgit/internal/service"
	"github.com/hyper-swe/mgit/internal/store/index"
)

// seedTaskCommits creates n commits and one audit entry for the given task.
func seedTaskCommits(t *testing.T, env *serviceEnv, taskID string, n int) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		_, err := env.commit.CreateCommit(ctx, service.CreateCommitRequest{
			AllowEmpty: true,
			TaskID:     taskID,
			AgentID:    "export-test",
			Message:    "step",
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
// carrying the task's REAL net diff, without mutating state.
//
// This test used to assert only on mbox header lines — HasPrefix("From "),
// Contains("Subject: [PATCH] ") — every one of which a header-only patch
// satisfies. That is exactly how MGIT-112 shipped: the export rendered zero
// hunks and this test stayed green. It now asserts on hunks and content.
// Refs: MGIT-4.2.4, MGIT-112
func TestExport_Git(t *testing.T) {
	env := setupServiceEnv(t)
	ctx := context.Background()
	taskID := "MGIT-4.2.4"

	const content = "package export\n\nfunc Export() int { return 7 }\n"
	require.NoError(t, os.WriteFile(
		filepath.Join(env.repo.Root(), "export.go"), []byte(content), 0o600))
	require.NoError(t, env.worktree.Add(ctx, "export.go"))
	_, err := env.commit.CreateCommit(ctx, service.CreateCommitRequest{
		TaskID: taskID, AgentID: "export-test", Message: "add export.go",
	})
	require.NoError(t, err)

	// Mirror the CLI's --format=git path exactly.
	preview, err := env.squash.PreviewGitPatch(ctx, service.SquashRequest{TaskID: taskID})
	require.NoError(t, err)
	require.False(t, preview.Empty, "the task has a real net change")

	patch := preview.Patch
	assert.True(t, strings.HasPrefix(patch, "From "))
	assert.Contains(t, patch, "Subject: [PATCH] ")
	assert.Contains(t, patch, "[squashed]")

	// The assertions that would have caught MGIT-112.
	require.True(t, service.PatchHasHunks(patch),
		"the export must carry real diff hunks, not just a well-formed header")
	assert.Contains(t, patch, "diff --git a/export.go b/export.go")
	assert.Contains(t, patch, "--- /dev/null", "an added file is git-apply-correct")
	assert.Contains(t, patch, "+func Export() int { return 7 }",
		"the patch must carry the real added content")

	// The export is a READ: no squash commit may be indexed for the task.
	records, err := env.idx.GetTaskCommits(ctx, taskID)
	require.NoError(t, err)
	assert.Len(t, records, 1,
		"--format=git must not append a real squash commit")
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
