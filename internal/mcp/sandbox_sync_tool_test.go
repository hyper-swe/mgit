package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// fakeSyncer stands in for the sandbox daemon client.
type fakeSyncer struct {
	task   string
	opts   model.WorktreeSyncOptions
	report *model.WorktreeSyncReport
	err    error
}

func (f *fakeSyncer) SyncWorktree(_ context.Context, taskID string, opts model.WorktreeSyncOptions) (*model.WorktreeSyncReport, error) {
	f.task, f.opts = taskID, opts
	return f.report, f.err
}

// syncServer wires a server whose sandbox connector returns the given syncer.
func syncServer(t *testing.T, s *fakeSyncer, connectErr error) *Server {
	t.Helper()
	return setupTestMCPWith(t, WithSandboxSync(func(context.Context) (SandboxSyncer, error) {
		if connectErr != nil {
			return nil, connectErr
		}
		return s, nil
	}))
}

// callSync invokes the tool with the given arguments.
func callSync(t *testing.T, srv *Server, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	var req mcp.CallToolRequest
	req.Params.Name = "mgit_sandbox_sync"
	req.Params.Arguments = args
	res, err := srv.sandboxSyncTool(context.Background(), req)
	require.NoError(t, err, "tool handlers report failures as results, never as transport errors")
	return res
}

// TestSandboxSyncTool_Delivers_ReturnsTheReport is the positive control: an
// agent calling over MCP gets the same classification the CLI prints.
// Refs: MGIT-76
func TestSandboxSyncTool_Delivers_ReturnsTheReport(t *testing.T) {
	syncer := &fakeSyncer{report: &model.WorktreeSyncReport{Updated: []string{"app.go"}}}
	srv := syncServer(t, syncer, nil)

	res := callSync(t, srv, map[string]any{"task_id": "MGIT-76"})

	assert.False(t, res.IsError)
	var got model.WorktreeSyncReport
	require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &got))
	assert.Equal(t, []string{"app.go"}, got.Updated)
	assert.Equal(t, "MGIT-76", syncer.task)
	assert.Equal(t, model.WorktreeSyncOptions{}, syncer.opts)
}

// TestSandboxSyncTool_DryRun_ReturnsTheConflictClassification is the point of
// MCP parity: an agent can discover which paths diverged WITHOUT running
// anything in the guest. Refs: MGIT-76
func TestSandboxSyncTool_DryRun_ReturnsTheConflictClassification(t *testing.T) {
	syncer := &fakeSyncer{report: &model.WorktreeSyncReport{
		DryRun: true, Refused: true,
		Conflicts: []model.WorktreeSyncConflict{{Path: "app.go", Reason: "modified in the guest"}},
	}}
	srv := syncServer(t, syncer, nil)

	res := callSync(t, srv, map[string]any{"task_id": "MGIT-76", "dry_run": true})

	assert.False(t, res.IsError, "a dry run reports; it does not fail")
	assert.True(t, syncer.opts.DryRun)
	var got model.WorktreeSyncReport
	require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &got))
	assert.True(t, got.Refused)
	require.Len(t, got.Conflicts, 1)
	assert.Equal(t, "app.go", got.Conflicts[0].Path)
}

// TestSandboxSyncTool_ForceReachesTheDaemon verifies the destructive switch is
// passed through rather than silently dropped.
func TestSandboxSyncTool_ForceReachesTheDaemon(t *testing.T) {
	syncer := &fakeSyncer{report: &model.WorktreeSyncReport{Overridden: []string{"app.go"}}}
	srv := syncServer(t, syncer, nil)

	callSync(t, srv, map[string]any{"task_id": "MGIT-76", "force": true})

	assert.True(t, syncer.opts.Force)
}

// TestSandboxSyncTool_Refusal_IsAToolErrorCarryingThePaths verifies a conflict
// is an ERROR result — an agent must not read a refusal as a completed sync —
// and that it still names what blocked it. Refs: MGIT-76
func TestSandboxSyncTool_Refusal_IsAToolErrorCarryingThePaths(t *testing.T) {
	syncer := &fakeSyncer{
		report: &model.WorktreeSyncReport{Refused: true,
			Conflicts: []model.WorktreeSyncConflict{{Path: "app.go", Reason: "modified in the guest"}}},
		err: model.ErrWorktreeSyncConflict,
	}
	srv := syncServer(t, syncer, nil)

	res := callSync(t, srv, map[string]any{"task_id": "MGIT-76"})

	assert.True(t, res.IsError)
	text := resultText(t, res)
	assert.Contains(t, text, "app.go")
	assert.Contains(t, text, "modified in the guest")
}

// TestSandboxSyncTool_HostileInput_IsRejectedBeforeTheDaemon verifies the
// GA-quality boundary rule holds for this tool too. Refs: MGIT-41, MGIT-76
func TestSandboxSyncTool_HostileInput_IsRejectedBeforeTheDaemon(t *testing.T) {
	for name, taskID := range map[string]string{
		"empty":          "",
		"path_separator": "../../etc/passwd",
		"shell_meta":     "MGIT-76; rm -rf /",
		"nul":            "MGIT-76\x00",
	} {
		t.Run(name, func(t *testing.T) {
			syncer := &fakeSyncer{report: &model.WorktreeSyncReport{}}
			srv := syncServer(t, syncer, nil)

			res := callSync(t, srv, map[string]any{"task_id": taskID})

			assert.True(t, res.IsError)
			assert.Empty(t, syncer.task, "hostile input must never reach the daemon")
		})
	}
}

// TestSandboxSyncTool_NoDaemon_IsAnHonestError verifies an unreachable daemon
// is reported rather than answered with an empty report an agent could read as
// "nothing to do". Refs: MGIT-76
func TestSandboxSyncTool_NoDaemon_IsAnHonestError(t *testing.T) {
	srv := syncServer(t, &fakeSyncer{}, assert.AnError)

	res := callSync(t, srv, map[string]any{"task_id": "MGIT-76"})

	assert.True(t, res.IsError)
	assert.NotContains(t, resultText(t, res), "\"skipped\"")
}

// TestSandboxSyncTool_Unwired_SaysSoRatherThanFakingSuccess verifies a server
// built without a sandbox connector reports the tool unavailable — never a
// fabricated success. Refs: MGIT-76
func TestSandboxSyncTool_Unwired_SaysSoRatherThanFakingSuccess(t *testing.T) {
	srv := setupTestMCP(t) // no WithSandboxSync

	res := callSync(t, srv, map[string]any{"task_id": "MGIT-76"})

	assert.True(t, res.IsError)
	assert.Contains(t, resultText(t, res), "daemon")
}
