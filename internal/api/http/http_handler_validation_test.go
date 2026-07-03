package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTaskCommits_Endpoint(t *testing.T) {
	srv := setupTestServer(t)

	// Create commit first
	body := `{"task_id":"MGIT-1.1","agent_id":"test","message":"for task query"}`
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v1/commits", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Echo().ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code)

	// Query by task
	req2 := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/v1/tasks/MGIT-1.1/commits", nil)
	rec2 := httptest.NewRecorder()
	srv.Echo().ServeHTTP(rec2, req2)
	assert.Equal(t, http.StatusOK, rec2.Code)
}

func TestCommit_PostCreate_BadJSON(t *testing.T) {
	srv := setupTestServer(t)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v1/commits", strings.NewReader("{bad}"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Echo().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestBranch_PostCreate_BadJSON(t *testing.T) {
	srv := setupTestServer(t)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v1/branches", strings.NewReader("{bad}"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Echo().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestSquash_BadJSON(t *testing.T) {
	srv := setupTestServer(t)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v1/squash", strings.NewReader("{bad}"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Echo().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestRollback_BadJSON(t *testing.T) {
	srv := setupTestServer(t)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v1/rollback", strings.NewReader("{bad}"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Echo().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}
