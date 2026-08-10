package service

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// exportingManager is a sandbox backend that CAN export artifacts. It writes
// the destination the way a real backend would, so the service's audit-failure
// rollback has something real to undo.
type exportingManager struct {
	fakeSandboxManager
	calls     int
	lastID    string
	lastReq   model.ArtifactExportRequest
	exportErr error
}

func (m *exportingManager) ExportArtifact(_ context.Context, id string,
	req model.ArtifactExportRequest) (*model.ArtifactExportResult, error) {
	m.calls++
	m.lastID, m.lastReq = id, req
	if m.exportErr != nil {
		return nil, m.exportErr
	}
	if err := os.WriteFile(req.HostPath, []byte("artifact\n"), 0o600); err != nil {
		return nil, err
	}
	manifest := req.HostPath + ".mgit-export.json"
	if err := os.WriteFile(manifest, []byte("{}\n"), 0o600); err != nil {
		return nil, err
	}
	return &model.ArtifactExportResult{
		SandboxID: id, TaskID: "MGIT-73", GuestPath: req.GuestPath, HostPath: req.HostPath,
		ManifestPath: manifest, Files: 1, Bytes: 9, TreeHash: "deadbeef",
	}, nil
}

// runningExportSandbox registers and boots a sandbox for a task.
func runningExportSandbox(t *testing.T, svc *SandboxService, task string) string {
	t.Helper()
	_, err := svc.Register(context.Background(), regOpts(task, "/work/"+task))
	require.NoError(t, err)
	info, err := svc.EnsureRunning(context.Background(), task)
	require.NoError(t, err)
	return info.ID
}

func TestSandboxService_ExportArtifact_Success_IsAuditedWithTaskPathAndSize(t *testing.T) {
	mgr := &exportingManager{}
	ev := &fakeEventAppender{}
	svc := newSvc(t, mgr, ev)
	id := runningExportSandbox(t, svc, "MGIT-73")
	dest := filepath.Join(t.TempDir(), "node_modules")

	res, err := svc.ExportArtifact(context.Background(), "MGIT-73",
		model.ArtifactExportRequest{GuestPath: "node_modules", HostPath: dest})

	require.NoError(t, err)
	assert.Equal(t, id, mgr.lastID, "the service resolves task -> sandbox; the caller never names a sandbox ID")
	assert.Equal(t, dest, res.HostPath)

	require.Contains(t, ev.types(), model.EventArtifactExported,
		"a file crossing the boundary without an audit record defeats the trail")
	last := ev.events[len(ev.events)-1]
	assert.Equal(t, id, last.SandboxID)
	assert.Equal(t, "MGIT-73", last.TaskID, "the export is bound to its task")
	var detail map[string]any
	require.NoError(t, json.Unmarshal([]byte(last.Detail), &detail))
	assert.Equal(t, "node_modules", detail["guest_path"])
	assert.Equal(t, dest, detail["host_path"])
	assert.Equal(t, float64(9), detail["bytes"], "the audit record carries the byte count")
	assert.Equal(t, "deadbeef", detail["tree_hash"])
}

func TestSandboxService_ExportArtifact_AuditFailure_UndoesTheExport(t *testing.T) {
	mgr := &exportingManager{}
	// created + resumed are events 1 and 2; the export audit is the third.
	ev := &fakeEventAppender{failNth: 3}
	svc := newSvc(t, mgr, ev)
	runningExportSandbox(t, svc, "MGIT-73")
	dest := filepath.Join(t.TempDir(), "node_modules")

	_, err := svc.ExportArtifact(context.Background(), "MGIT-73",
		model.ArtifactExportRequest{GuestPath: "node_modules", HostPath: dest})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "audit")
	assert.NoFileExists(t, dest, "an un-auditable export must not survive on the host")
	assert.NoFileExists(t, dest+".mgit-export.json")
}

func TestSandboxService_ExportArtifact_BackendWithoutExport_FailsClosed(t *testing.T) {
	svc := newSvc(t, &fakeSandboxManager{}, &fakeEventAppender{})
	runningExportSandbox(t, svc, "MGIT-73")

	_, err := svc.ExportArtifact(context.Background(), "MGIT-73",
		model.ArtifactExportRequest{GuestPath: "out", HostPath: filepath.Join(t.TempDir(), "x")})

	require.ErrorIs(t, err, model.ErrArtifactExportUnsupported)
}

func TestSandboxService_ExportArtifact_UnknownTask_ReturnsNotFound(t *testing.T) {
	svc := newSvc(t, &exportingManager{}, &fakeEventAppender{})

	_, err := svc.ExportArtifact(context.Background(), "MGIT-99",
		model.ArtifactExportRequest{GuestPath: "out", HostPath: filepath.Join(t.TempDir(), "x")})

	require.ErrorIs(t, err, model.ErrSandboxNotFound)
}

func TestSandboxService_ExportArtifact_UnbootedSandbox_IsRefused(t *testing.T) {
	svc := newSvc(t, &exportingManager{}, &fakeEventAppender{})
	_, err := svc.Register(context.Background(), regOpts("MGIT-73", "/work/a"))
	require.NoError(t, err)

	_, err = svc.ExportArtifact(context.Background(), "MGIT-73",
		model.ArtifactExportRequest{GuestPath: "out", HostPath: filepath.Join(t.TempDir(), "x")})

	require.ErrorIs(t, err, model.ErrSandboxBackendUnavailable)
}

func TestSandboxService_ExportArtifact_InvalidRequest_IsRejectedBeforeTheBackend(t *testing.T) {
	tests := []struct {
		name string
		req  model.ArtifactExportRequest
	}{
		{name: "empty_guest_path", req: model.ArtifactExportRequest{HostPath: "/tmp/x"}},
		{name: "absolute_guest_path", req: model.ArtifactExportRequest{GuestPath: "/etc/passwd", HostPath: "/tmp/x"}},
		{name: "nul_in_guest_path", req: model.ArtifactExportRequest{GuestPath: "a\x00b", HostPath: "/tmp/x"}},
		{name: "empty_host_path", req: model.ArtifactExportRequest{GuestPath: "out"}},
		{name: "relative_host_path", req: model.ArtifactExportRequest{GuestPath: "out", HostPath: "rel/path"}},
		{name: "nul_in_host_path", req: model.ArtifactExportRequest{GuestPath: "out", HostPath: "/tmp/a\x00b"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr := &exportingManager{}
			svc := newSvc(t, mgr, &fakeEventAppender{})
			runningExportSandbox(t, svc, "MGIT-73")

			_, err := svc.ExportArtifact(context.Background(), "MGIT-73", tt.req)

			require.Error(t, err)
			assert.Zero(t, mgr.calls, "an invalid request must never reach the backend")
		})
	}
}

func TestSandboxService_ExportArtifact_BackendRefusal_IsNotAudited(t *testing.T) {
	mgr := &exportingManager{exportErr: errors.New("symlink escapes the exported subtree")}
	ev := &fakeEventAppender{}
	svc := newSvc(t, mgr, ev)
	runningExportSandbox(t, svc, "MGIT-73")

	_, err := svc.ExportArtifact(context.Background(), "MGIT-73",
		model.ArtifactExportRequest{GuestPath: "out", HostPath: filepath.Join(t.TempDir(), "x")})

	require.Error(t, err)
	assert.NotContains(t, ev.types(), model.EventArtifactExported,
		"nothing crossed the boundary, so nothing is recorded as having crossed it")
}
