package sandboxd

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/controlproto"
	"github.com/hyper-swe/mgit/internal/model"
)

// fakeExporter records the export requests the daemon routes to it.
type fakeExporter struct {
	calls   int
	taskID  string
	req     model.ArtifactExportRequest
	failure error
}

func (f *fakeExporter) ExportArtifact(_ context.Context, taskID string,
	req model.ArtifactExportRequest) (*model.ArtifactExportResult, error) {
	f.calls++
	f.taskID, f.req = taskID, req
	if f.failure != nil {
		return nil, f.failure
	}
	return &model.ArtifactExportResult{
		SandboxID: "01JXSBSANDBOX", TaskID: taskID, GuestPath: req.GuestPath, HostPath: req.HostPath,
		ManifestPath: req.HostPath + ".mgit-export.json", Files: 2, Bytes: 64, TreeHash: "beef",
	}, nil
}

// exportRequest builds a well-formed export control request.
func exportRequest(taskID, guestPath, hostPath string) *controlproto.Request {
	return &controlproto.Request{
		Kind: controlproto.KindExport,
		Export: &controlproto.ExportArgs{TaskID: taskID, Export: model.ArtifactExportRequest{
			GuestPath: guestPath, HostPath: hostPath,
		}},
	}
}

func TestDaemon_Export_RoutesToTheExporter(t *testing.T) {
	skipUnsupportedHostIPC(t)
	exporter := &fakeExporter{}
	cfg, _ := dispatchConfig(t, &fakeDispatcher{})
	cfg.Exporter = exporter
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runDaemon(ctx, t, cfg)

	conn := dialAuthed(t, cfg.SocketPath)
	defer func() { _ = conn.Close() }()
	require.NoError(t, controlproto.WriteRequest(conn,
		exportRequest("MGIT-73", "node_modules", "/tmp/mgit-export-dest")))

	resp, err := controlproto.ReadResponse(conn)
	require.NoError(t, err)
	require.Empty(t, resp.Error)
	require.NotNil(t, resp.Exported)
	assert.Equal(t, "MGIT-73", exporter.taskID)
	assert.Equal(t, "node_modules", exporter.req.GuestPath)
	assert.Equal(t, "/tmp/mgit-export-dest", resp.Exported.HostPath)
	assert.Equal(t, "beef", resp.Exported.TreeHash)

	cancel()
	require.NoError(t, <-done)
}

func TestDaemon_Export_NotWired_ReportsUnserved(t *testing.T) {
	skipUnsupportedHostIPC(t)
	cfg, _ := dispatchConfig(t, &fakeDispatcher{}) // no Exporter
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runDaemon(ctx, t, cfg)

	conn := dialAuthed(t, cfg.SocketPath)
	defer func() { _ = conn.Close() }()
	require.NoError(t, controlproto.WriteRequest(conn,
		exportRequest("MGIT-73", "node_modules", "/tmp/mgit-export-dest")))

	resp, err := controlproto.ReadResponse(conn)
	require.NoError(t, err)
	assert.Contains(t, resp.Error, "not served by this daemon",
		"an unwired export verb says so rather than silently succeeding")

	cancel()
	require.NoError(t, <-done)
}

func TestDaemon_Export_Refusal_SurfacesAsAResponseError(t *testing.T) {
	skipUnsupportedHostIPC(t)
	cfg, _ := dispatchConfig(t, &fakeDispatcher{})
	cfg.Exporter = &fakeExporter{failure: errors.New("artifact export path is unsafe: symlink escapes")}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runDaemon(ctx, t, cfg)

	conn := dialAuthed(t, cfg.SocketPath)
	defer func() { _ = conn.Close() }()
	require.NoError(t, controlproto.WriteRequest(conn,
		exportRequest("MGIT-73", "node_modules", "/tmp/mgit-export-dest")))

	resp, err := controlproto.ReadResponse(conn)
	require.NoError(t, err)
	assert.Contains(t, resp.Error, "unsafe")
	assert.Nil(t, resp.Exported)

	cancel()
	require.NoError(t, <-done)
}

func TestCeilingManager_ExportArtifact_DelegatesOrReportsUnsupported(t *testing.T) {
	t.Run("backend_without_export", func(t *testing.T) {
		mgr := NewCeilingManager(&launchingManager{fakeManager: *newFakeManager()}, 2, 0, 0)

		_, err := mgr.ExportArtifact(context.Background(), "sbx-1",
			model.ArtifactExportRequest{GuestPath: "out", HostPath: "/tmp/x"})

		require.ErrorIs(t, err, model.ErrArtifactExportUnsupported)
	})

	t.Run("backend_with_export", func(t *testing.T) {
		inner := &exportingCeilingManager{launchingManager: launchingManager{fakeManager: *newFakeManager()}}
		mgr := NewCeilingManager(inner, 2, 0, 0)

		res, err := mgr.ExportArtifact(context.Background(), "sbx-1",
			model.ArtifactExportRequest{GuestPath: "out", HostPath: "/tmp/x"})

		require.NoError(t, err)
		assert.Equal(t, "/tmp/x", res.HostPath, "the ceiling forwards the capability unchanged")
	})
}

// exportingCeilingManager is a ceiling-wrapped backend that CAN export, so the
// passthrough is proven rather than assumed.
type exportingCeilingManager struct {
	launchingManager
}

func (m *exportingCeilingManager) ExportArtifact(_ context.Context, id string,
	req model.ArtifactExportRequest) (*model.ArtifactExportResult, error) {
	return &model.ArtifactExportResult{SandboxID: id, HostPath: req.HostPath}, nil
}
