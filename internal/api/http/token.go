package http

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/labstack/echo/v4"
)

// TokenStore manages API tokens stored in .mgit/tokens.json.
// Tokens are stored as SHA-256 hashes — plaintext is shown once at generation.
// Refs: NFR-5.11, MGIT-5.1.5
type TokenStore struct {
	path   string
	tokens []TokenEntry
	mu     sync.RWMutex
}

// TokenEntry represents a stored token (hash only, never plaintext).
// Refs: NFR-5.11
type TokenEntry struct {
	Hash      string `json:"hash"`
	CreatedAt string `json:"created_at"`
	ExpiresAt string `json:"expires_at"`
	Revoked   bool   `json:"revoked"`
}

// NewTokenStore loads or creates the token store at the given path.
func NewTokenStore(path string) (*TokenStore, error) {
	ts := &TokenStore{path: path}
	if data, err := os.ReadFile(path); err == nil { //nolint:gosec // internal path
		var wrapper struct {
			Tokens []TokenEntry `json:"tokens"`
		}
		if err := json.Unmarshal(data, &wrapper); err == nil {
			ts.tokens = wrapper.Tokens
		}
	}
	return ts, nil
}

// Generate creates a new token, stores the hash, returns plaintext (shown once).
// Refs: NFR-5.11
func (ts *TokenStore) Generate(expiryDays int) (string, error) {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	// Generate 32 cryptographically random bytes
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return "", fmt.Errorf("generate random token: %w", err)
	}
	plaintext := base64.URLEncoding.EncodeToString(tokenBytes)

	// Store SHA-256 hash only
	hash := sha256.Sum256([]byte(plaintext))
	hashHex := fmt.Sprintf("%x", hash)

	now := time.Now().UTC()
	entry := TokenEntry{
		Hash:      hashHex,
		CreatedAt: now.Format(time.RFC3339),
		ExpiresAt: now.Add(time.Duration(expiryDays) * 24 * time.Hour).Format(time.RFC3339),
		Revoked:   false,
	}
	ts.tokens = append(ts.tokens, entry)

	if err := ts.save(); err != nil {
		return "", err
	}
	return plaintext, nil
}

// Rotate generates a new token and revokes all previous ones.
func (ts *TokenStore) Rotate(expiryDays int) (string, error) {
	ts.mu.Lock()
	for i := range ts.tokens {
		ts.tokens[i].Revoked = true
	}
	ts.mu.Unlock()

	return ts.Generate(expiryDays)
}

// Revoke marks all active tokens as revoked.
func (ts *TokenStore) Revoke() error {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	for i := range ts.tokens {
		ts.tokens[i].Revoked = true
	}
	return ts.save()
}

// List returns all tokens with masked values.
func (ts *TokenStore) List() []map[string]string {
	ts.mu.RLock()
	defer ts.mu.RUnlock()

	result := make([]map[string]string, 0, len(ts.tokens))
	for _, t := range ts.tokens {
		masked := "****..." + t.Hash[len(t.Hash)-8:]
		status := "active"
		if t.Revoked {
			status = "revoked"
		}
		result = append(result, map[string]string{
			"token":      masked,
			"created_at": t.CreatedAt,
			"expires_at": t.ExpiresAt,
			"status":     status,
		})
	}
	return result
}

// ValidateToken checks if a plaintext token is valid (not expired, not revoked).
func (ts *TokenStore) ValidateToken(plaintext string) bool {
	ts.mu.RLock()
	defer ts.mu.RUnlock()

	hash := sha256.Sum256([]byte(plaintext))
	hashHex := fmt.Sprintf("%x", hash)

	now := time.Now().UTC()
	for _, t := range ts.tokens {
		if t.Hash == hashHex && !t.Revoked {
			expiry, err := time.Parse(time.RFC3339, t.ExpiresAt)
			if err != nil {
				continue
			}
			if now.Before(expiry) {
				return true
			}
		}
	}
	return false
}

// save writes the token store to disk with 0600 permissions.
func (ts *TokenStore) save() error {
	data, err := json.MarshalIndent(struct {
		Tokens []TokenEntry `json:"tokens"`
	}{Tokens: ts.tokens}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal tokens: %w", err)
	}
	return os.WriteFile(ts.path, data, 0o600)
}

// BearerAuthMiddleware validates Bearer tokens on API requests.
// Refs: NFR-5.11
func BearerAuthMiddleware(ts *TokenStore) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			// Skip auth for health endpoint
			if c.Path() == "/health" {
				return next(c)
			}

			auth := c.Request().Header.Get("Authorization")
			if auth == "" || !strings.HasPrefix(auth, "Bearer ") {
				return c.JSON(http.StatusUnauthorized, map[string]string{
					"error": "missing or invalid Authorization header",
				})
			}

			token := strings.TrimPrefix(auth, "Bearer ")
			if !ts.ValidateToken(token) {
				return c.JSON(http.StatusUnauthorized, map[string]string{
					"error": "invalid or expired token",
				})
			}

			return next(c)
		}
	}
}
