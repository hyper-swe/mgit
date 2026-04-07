package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTokenGenerate_CreatesTokenFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.json")
	ts, err := NewTokenStore(path)
	require.NoError(t, err)

	plaintext, err := ts.Generate(90)
	require.NoError(t, err)
	assert.NotEmpty(t, plaintext)
	assert.FileExists(t, path)
}

func TestTokenGenerate_HashStoredNotPlaintext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.json")
	ts, err := NewTokenStore(path)
	require.NoError(t, err)

	plaintext, err := ts.Generate(90)
	require.NoError(t, err)

	// Reload and verify plaintext is NOT stored
	ts2, err := NewTokenStore(path)
	require.NoError(t, err)
	for _, tok := range ts2.tokens {
		assert.NotEqual(t, plaintext, tok.Hash, "plaintext must not be stored")
		assert.Len(t, tok.Hash, 64, "hash must be SHA-256 hex (64 chars)")
	}
}

func TestTokenRotate_InvalidatesOld(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.json")
	ts, err := NewTokenStore(path)
	require.NoError(t, err)

	old, err := ts.Generate(90)
	require.NoError(t, err)

	newToken, err := ts.Rotate(90)
	require.NoError(t, err)
	assert.NotEqual(t, old, newToken)
	assert.False(t, ts.ValidateToken(old), "old token must be invalid after rotate")
	assert.True(t, ts.ValidateToken(newToken), "new token must be valid")
}

func TestTokenRevoke_MarksRevoked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.json")
	ts, err := NewTokenStore(path)
	require.NoError(t, err)

	plaintext, err := ts.Generate(90)
	require.NoError(t, err)
	assert.True(t, ts.ValidateToken(plaintext))

	require.NoError(t, ts.Revoke())
	assert.False(t, ts.ValidateToken(plaintext), "revoked token must be invalid")
}

func TestTokenList_MaskedOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.json")
	ts, err := NewTokenStore(path)
	require.NoError(t, err)

	_, err = ts.Generate(90)
	require.NoError(t, err)

	list := ts.List()
	require.Len(t, list, 1)
	assert.Contains(t, list[0]["token"], "****...", "token must be masked")
	assert.NotEmpty(t, list[0]["expires_at"])
	assert.Equal(t, "active", list[0]["status"])
}

func TestAuthMiddleware_ValidToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.json")
	ts, err := NewTokenStore(path)
	require.NoError(t, err)

	plaintext, err := ts.Generate(90)
	require.NoError(t, err)

	e := echo.New()
	e.Use(BearerAuthMiddleware(ts))
	e.GET("/api/test", func(c echo.Context) error {
		return c.JSON(http.StatusOK, map[string]string{"ok": "true"})
	})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/test", nil)
	req.Header.Set("Authorization", "Bearer "+plaintext)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestAuthMiddleware_MissingToken_Returns401(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.json")
	ts, err := NewTokenStore(path)
	require.NoError(t, err)

	e := echo.New()
	e.Use(BearerAuthMiddleware(ts))
	e.GET("/api/test", func(c echo.Context) error {
		return c.JSON(http.StatusOK, nil)
	})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/test", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestAuthMiddleware_ExpiredToken_Returns401(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.json")
	ts, err := NewTokenStore(path)
	require.NoError(t, err)

	// Generate with 0 days expiry (already expired)
	plaintext, err := ts.Generate(0)
	require.NoError(t, err)

	e := echo.New()
	e.Use(BearerAuthMiddleware(ts))
	e.GET("/api/test", func(c echo.Context) error {
		return c.JSON(http.StatusOK, nil)
	})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/test", nil)
	req.Header.Set("Authorization", "Bearer "+plaintext)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestAuthMiddleware_HealthSkipsAuth(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.json")
	ts, err := NewTokenStore(path)
	require.NoError(t, err)

	e := echo.New()
	e.Use(BearerAuthMiddleware(ts))
	e.GET("/health", func(c echo.Context) error {
		return c.JSON(http.StatusOK, map[string]string{"status": "ok"})
	})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
}
