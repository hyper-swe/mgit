package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gitstore "github.com/hyper-swe/mgit-dev/internal/store/git"
	"github.com/hyper-swe/mgit-dev/internal/store/index"
)

func fixedClock() func() time.Time {
	return func() time.Time { return time.Date(2026, 4, 7, 12, 0, 0, 0, time.UTC) }
}

func setupTestServer(t *testing.T) *Server {
	t.Helper()
	tmpDir := t.TempDir()
	clock := fixedClock()

	repo, err := gitstore.Init(tmpDir, clock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = repo.Close() })

	dbPath := filepath.Join(tmpDir, ".mgit", "index.db")
	idx, err := index.New(dbPath, clock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = idx.Close() })

	return NewServer(repo, idx, clock)
}

func TestHealth_Endpoint(t *testing.T) {
	srv := setupTestServer(t)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()

	srv.Echo().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "ok", body["status"])
	assert.NotEmpty(t, body["timestamp"])
}

func TestCommit_PostCreate(t *testing.T) {
	srv := setupTestServer(t)
	body := `{"task_id":"MGIT-1.2.3","agent_id":"test","message":"test commit"}`
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v1/commits", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv.Echo().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusCreated, rec.Code)
	var result map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &result))
	assert.NotEmpty(t, result["commit_id"])
}

func TestCommit_GetList(t *testing.T) {
	srv := setupTestServer(t)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/v1/commits", nil)
	rec := httptest.NewRecorder()

	srv.Echo().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestCommit_GetById_NotFound(t *testing.T) {
	srv := setupTestServer(t)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/v1/commits/0000000000000000000000000000000000000000", nil)
	rec := httptest.NewRecorder()

	srv.Echo().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestBranch_GetList(t *testing.T) {
	srv := setupTestServer(t)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/v1/branches", nil)
	rec := httptest.NewRecorder()

	srv.Echo().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestBranch_PostCreate(t *testing.T) {
	srv := setupTestServer(t)
	body := `{"task_id":"MGIT-2.1"}`
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v1/branches", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv.Echo().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusCreated, rec.Code)
}

func TestVerify_Endpoint(t *testing.T) {
	srv := setupTestServer(t)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/v1/verify", nil)
	rec := httptest.NewRecorder()

	srv.Echo().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	var result map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &result))
	assert.NotNil(t, result["ok"])
}

func TestMiddleware_RequestID(t *testing.T) {
	srv := setupTestServer(t)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()

	srv.Echo().ServeHTTP(rec, req)

	assert.NotEmpty(t, rec.Header().Get("X-Request-Id"))
}

func TestSquash_Endpoint(t *testing.T) {
	srv := setupTestServer(t)

	// First create a commit
	body := `{"task_id":"MGIT-3.1","agent_id":"test","message":"pre-squash"}`
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v1/commits", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Echo().ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code)

	// Squash
	squashBody := `{"task_id":"MGIT-3.1"}`
	req2 := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v1/squash", strings.NewReader(squashBody))
	req2.Header.Set("Content-Type", "application/json")
	rec2 := httptest.NewRecorder()
	srv.Echo().ServeHTTP(rec2, req2)

	assert.Equal(t, http.StatusCreated, rec2.Code)
}

func TestRollback_Endpoint(t *testing.T) {
	srv := setupTestServer(t)

	// Create a commit first
	body := `{"task_id":"MGIT-4.1","agent_id":"test","message":"to rollback"}`
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v1/commits", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Echo().ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code)

	// Rollback
	rbBody := `{"task_id":"MGIT-4.1"}`
	req2 := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v1/rollback", strings.NewReader(rbBody))
	req2.Header.Set("Content-Type", "application/json")
	rec2 := httptest.NewRecorder()
	srv.Echo().ServeHTTP(rec2, req2)

	assert.Equal(t, http.StatusCreated, rec2.Code)
}
