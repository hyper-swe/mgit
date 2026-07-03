package http

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gitstore "github.com/hyper-swe/mgit/internal/store/git"
	"github.com/hyper-swe/mgit/internal/store/index"
	"github.com/hyper-swe/mgit/internal/store/lock"
)

// TestREST_NewServer_WithLocker_ServesUnderLock wires a locker through the real
// constructor (WithLocker + setupMiddleware) and confirms the server still
// serves a request and releases the lock afterward. Refs: MGIT-46
func TestREST_NewServer_WithLocker_ServesUnderLock(t *testing.T) {
	tmpDir := t.TempDir()
	clock := func() time.Time { return time.Now().UTC() }
	repo, err := gitstore.Init(tmpDir, clock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = repo.Close() })
	idx, err := index.New(filepath.Join(tmpDir, ".mgit", "index.db"), clock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = idx.Close() })

	storeDir := filepath.Join(tmpDir, ".mgit")
	srv := NewServer(repo, idx, clock, WithLocker(lock.NewGuarder(storeDir, lock.DefaultTimeout)))

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	srv.echo.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)

	// The per-request lock was released; the CLI can acquire immediately.
	lk, err := lock.Acquire(storeDir, 0)
	require.NoError(t, err)
	require.NoError(t, lk.Release())
}

// TestREST_LockingMiddleware_GuardsRequest proves the middleware runs the
// request handler while holding the repo lock and releases it afterward.
// Refs: MGIT-46
func TestREST_LockingMiddleware_GuardsRequest(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".mgit")
	s := &Server{echo: echo.New(), locker: lock.NewGuarder(dir, lock.DefaultTimeout)}

	heldDuringHandler := false
	handler := s.lockingMiddleware()(func(c echo.Context) error {
		_, err := lock.Acquire(dir, 0)
		heldDuringHandler = errors.Is(err, lock.ErrLockHeld)
		return c.String(http.StatusOK, "ok")
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	require.NoError(t, handler(s.echo.NewContext(req, rec)))
	assert.True(t, heldDuringHandler, "request handler must run while the repo lock is held")
	assert.Equal(t, http.StatusOK, rec.Code)

	lk, err := lock.Acquire(dir, 0)
	require.NoError(t, err, "lock not released after the request")
	require.NoError(t, lk.Release())
}

// TestREST_LockingMiddleware_AcquireFailure surfaces a contended lock as 503
// Service Unavailable rather than hanging the request. Refs: MGIT-46
func TestREST_LockingMiddleware_AcquireFailure(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".mgit")
	held, err := lock.Acquire(dir, lock.DefaultTimeout)
	require.NoError(t, err)
	defer func() { _ = held.Release() }()

	s := &Server{echo: echo.New(), locker: lock.NewGuarder(dir, 0)}
	called := false
	handler := s.lockingMiddleware()(func(c echo.Context) error {
		called = true
		return c.String(http.StatusOK, "ok")
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	err = handler(s.echo.NewContext(req, rec))

	var httpErr *echo.HTTPError
	require.ErrorAs(t, err, &httpErr)
	assert.Equal(t, http.StatusServiceUnavailable, httpErr.Code)
	assert.False(t, called, "handler must not run when the lock cannot be acquired")
}
