package mtix

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit-dev/internal/model"
	"github.com/hyper-swe/mgit-dev/internal/service"
	gitstore "github.com/hyper-swe/mgit-dev/internal/store/git"
	"github.com/hyper-swe/mgit-dev/internal/store/index"
)

func setupIntegration(t *testing.T, mtixSrv *httptest.Server) *Integration {
	t.Helper()
	tmpDir := t.TempDir()
	clock := func() time.Time { return time.Date(2026, 4, 7, 12, 0, 0, 0, time.UTC) }

	repo, err := gitstore.Init(tmpDir, clock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = repo.Close() })

	dbPath := filepath.Join(tmpDir, ".mgit", "index.db")
	idx, err := index.New(dbPath, clock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = idx.Close() })

	cs := gitstore.NewCommitStore(repo)
	squashSvc := service.NewSquashService(repo, cs, idx)
	client := NewClient(mtixSrv.URL, "test-agent")
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	return NewIntegration(client, idx, squashSvc, logger)
}

func TestIntegration_SyncTaskCommit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(Node{ID: "MGIT-1.2.3", Status: "open"})
	}))
	defer srv.Close()

	integration := setupIntegration(t, srv)
	ctx := context.Background()

	tid, _ := model.ParseTaskID("MGIT-1.2.3")
	commit := &model.Commit{
		CommitID: "abc123def456abc123def456abc123def456abc1",
		TaskID:   tid,
		Message:  "test commit",
	}

	err := integration.SyncTaskCommit(ctx, "MGIT-1.2.3", commit)
	assert.NoError(t, err)
}

func TestIntegration_SyncTaskCommit_MtixUnreachable(t *testing.T) {
	// mtix down — sync should still succeed (graceful degradation)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	integration := setupIntegration(t, srv)
	ctx := context.Background()

	tid, _ := model.ParseTaskID("MGIT-2.1")
	commit := &model.Commit{CommitID: "def456", TaskID: tid}

	err := integration.SyncTaskCommit(ctx, "MGIT-2.1", commit)
	assert.NoError(t, err, "sync must succeed even when mtix is down")
}

func TestIntegration_GetTaskInfo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(Node{ID: "MGIT-3.1", Title: "Feature X", Status: "in_progress"})
	}))
	defer srv.Close()

	integration := setupIntegration(t, srv)
	ctx := context.Background()

	node, err := integration.GetTaskInfo(ctx, "MGIT-3.1")
	require.NoError(t, err)
	assert.Equal(t, "Feature X", node.Title)
	assert.Equal(t, "in_progress", node.Status)
}

func TestIntegration_OnTaskDone_AutoSquash(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(Node{ID: "MGIT-4.1", Status: "done"})
	}))
	defer srv.Close()

	// Setup with real stores for squash
	tmpDir := t.TempDir()
	clock := func() time.Time { return time.Date(2026, 4, 7, 12, 0, 0, 0, time.UTC) }

	repo, err := gitstore.Init(tmpDir, clock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = repo.Close() })

	dbPath := filepath.Join(tmpDir, ".mgit", "index.db")
	idx, err := index.New(dbPath, clock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = idx.Close() })

	cs := gitstore.NewCommitStore(repo)
	commitSvc := service.NewCommitService(repo, cs, idx)
	squashSvc := service.NewSquashService(repo, cs, idx)
	client := NewClient(srv.URL, "test-agent")
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	integration := NewIntegration(client, idx, squashSvc, logger)

	ctx := context.Background()

	// Create commits first
	for i := range 3 {
		_, err := commitSvc.CreateCommit(ctx, service.CreateCommitRequest{
			TaskID: "MGIT-4.1", AgentID: "test", Message: string(rune('A' + i)),
		})
		require.NoError(t, err)
	}

	// Trigger auto-squash via OnTaskDone
	err = integration.OnTaskDone(ctx, "MGIT-4.1")
	require.NoError(t, err)

	// Verify squash happened (4 entries: 3 original + 1 squash)
	records, err := idx.GetTaskCommits(ctx, "MGIT-4.1")
	require.NoError(t, err)
	assert.Len(t, records, 4)
}
