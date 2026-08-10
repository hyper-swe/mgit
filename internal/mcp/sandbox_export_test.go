package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
	gitstore "github.com/hyper-swe/mgit/internal/store/git"
	"github.com/hyper-swe/mgit/internal/store/index"
)

// fakeExportClient stands in for the sandbox daemon.
type fakeExportClient struct {
	calls   int
	taskID  string
	req     model.ArtifactExportRequest
	failure error
}

func (f *fakeExportClient) ExportArtifact(_ context.Context, taskID string,
	req model.ArtifactExportRequest) (*model.ArtifactExportResult, error) {
	f.calls++
	f.taskID, f.req = taskID, req
	if f.failure != nil {
		return nil, f.failure
	}
	return &model.ArtifactExportResult{
		SandboxID: "01JSB", TaskID: taskID, GuestPath: req.GuestPath, HostPath: req.HostPath,
		ManifestPath: req.HostPath + ".mgit-export.json", Files: 7, Bytes: 2048, TreeHash: "cafe",
	}, nil
}

// exportServer builds an MCP server wired to a fake sandbox daemon.
func exportServer(t *testing.T, client SandboxExportClient, connectErr error) *Server {
	t.Helper()
	tmpDir := t.TempDir()
	clock := fixedClock()
	repo, err := gitstore.Init(tmpDir, clock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = repo.Close() })
	idx, err := index.New(filepath.Join(tmpDir, ".mgit", "index.db"), clock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = idx.Close() })
	return NewServer(repo, idx, WithSandboxExport(func(context.Context) (SandboxExportClient, error) {
		if connectErr != nil {
			return nil, connectErr
		}
		return client, nil
	}))
}

func TestSandboxExportTool_ValidRequest_DelegatesAndReportsWhatCrossed(t *testing.T) {
	fake := &fakeExportClient{}
	srv := exportServer(t, fake, nil)
	dest := filepath.Join(t.TempDir(), "node_modules")

	res, err := srv.sandboxExportTool(context.Background(), makeToolReq(map[string]any{
		"task_id": "MGIT-73", "guest_path": "node_modules", "host_path": dest,
	}))

	require.NoError(t, err)
	require.False(t, res.IsError, resultText(t, res))
	assert.Equal(t, "MGIT-73", fake.taskID)
	assert.Equal(t, "node_modules", fake.req.GuestPath)
	assert.Equal(t, dest, fake.req.HostPath)

	var out model.ArtifactExportResult
	require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &out))
	assert.Equal(t, 7, out.Files)
	assert.Equal(t, "cafe", out.TreeHash)
	assert.NotEmpty(t, out.ManifestPath, "the agent is told where the provenance record landed")
}

func TestSandboxExportTool_HostileArguments_RejectedBeforeTheDaemon(t *testing.T) {
	tests := []struct {
		name string
		args map[string]any
	}{
		{name: "traversing_guest_path", args: map[string]any{
			"task_id": "MGIT-73", "guest_path": "../../etc", "host_path": "/tmp/x"}},
		{name: "traversing_host_path", args: map[string]any{
			"task_id": "MGIT-73", "guest_path": "build", "host_path": "/tmp/../etc/shadow"}},
		{name: "nul_in_guest_path", args: map[string]any{
			"task_id": "MGIT-73", "guest_path": "build\x00", "host_path": "/tmp/x"}},
		{name: "empty_guest_path", args: map[string]any{
			"task_id": "MGIT-73", "guest_path": "", "host_path": "/tmp/x"}},
		{name: "empty_host_path", args: map[string]any{
			"task_id": "MGIT-73", "guest_path": "build", "host_path": ""}},
		{name: "hostile_task_id", args: map[string]any{
			"task_id": "MGIT-73; rm -rf /", "guest_path": "build", "host_path": "/tmp/x"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeExportClient{}
			srv := exportServer(t, fake, nil)

			res, err := srv.sandboxExportTool(context.Background(), makeToolReq(tt.args))

			require.NoError(t, err)
			assert.True(t, res.IsError, "hostile input must come back as a structured tool error")
			assert.Zero(t, fake.calls, "hostile input must never reach the sandbox daemon")
		})
	}
}

func TestSandboxExportTool_NoDaemonWired_FailsClosedWithTheReason(t *testing.T) {
	srv := setupTestMCP(t) // no WithSandboxExport

	res, err := srv.sandboxExportTool(context.Background(), makeToolReq(map[string]any{
		"task_id": "MGIT-73", "guest_path": "build", "host_path": "/tmp/x",
	}))

	require.NoError(t, err)
	require.True(t, res.IsError)
	assert.Contains(t, resultText(t, res), "not wired to a sandbox daemon",
		"an unavailable capability must say so, never fake a success")
}

func TestSandboxExportTool_DaemonUnavailable_SurfacesTheReason(t *testing.T) {
	srv := exportServer(t, nil, errors.New("sandbox daemon unavailable (no fallback)"))

	res, err := srv.sandboxExportTool(context.Background(), makeToolReq(map[string]any{
		"task_id": "MGIT-73", "guest_path": "build", "host_path": "/tmp/x",
	}))

	require.NoError(t, err)
	require.True(t, res.IsError)
	assert.Contains(t, resultText(t, res), "daemon unavailable")
}

func TestSandboxExportTool_DaemonRefusal_SurfacesTheRefusal(t *testing.T) {
	fake := &fakeExportClient{failure: errors.New("artifact export destination already exists")}
	srv := exportServer(t, fake, nil)

	res, err := srv.sandboxExportTool(context.Background(), makeToolReq(map[string]any{
		"task_id": "MGIT-73", "guest_path": "build", "host_path": "/tmp/x",
	}))

	require.NoError(t, err)
	require.True(t, res.IsError)
	assert.Contains(t, resultText(t, res), "already exists")
}
