package mcp

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	mcptypes "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gitstore "github.com/hyper-swe/mgit/internal/store/git"
	"github.com/hyper-swe/mgit/internal/store/index"
	"github.com/hyper-swe/mgit/internal/store/lock"
)

// TestMCP_LockingMiddleware_GuardsHandler proves the middleware runs the tool
// handler while holding the repo lock and releases it afterward — the
// per-operation locking that lets serve coexist with the CLI. Refs: MGIT-46
func TestMCP_LockingMiddleware_GuardsHandler(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".mgit")
	g := lock.NewGuarder(dir, lock.DefaultTimeout)
	mw := lockingMiddleware(g)

	heldDuringHandler := false
	next := func(_ context.Context, _ mcptypes.CallToolRequest) (*mcptypes.CallToolResult, error) {
		// A non-blocking acquire must fail while the middleware holds the lock.
		_, err := lock.Acquire(dir, 0)
		heldDuringHandler = errors.Is(err, lock.ErrLockHeld)
		return mcptypes.NewToolResultText("ok"), nil
	}

	res, err := mw(next)(context.Background(), makeToolReq(map[string]any{}))
	require.NoError(t, err)
	require.False(t, res.IsError)
	assert.True(t, heldDuringHandler, "tool handler must run while the repo lock is held")

	// Released after the call: the CLI can acquire immediately.
	lk, err := lock.Acquire(dir, 0)
	require.NoError(t, err, "lock not released after the tool call")
	require.NoError(t, lk.Release())
}

// TestMCP_LockingMiddleware_AcquireFailure surfaces a contended lock as a
// structured tool error (not a hang, not a crash) when it cannot be acquired
// within the timeout. Refs: MGIT-46
func TestMCP_LockingMiddleware_AcquireFailure(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".mgit")
	// Hold the lock so the middleware's 0-timeout acquire fails.
	held, err := lock.Acquire(dir, lock.DefaultTimeout)
	require.NoError(t, err)
	defer func() { _ = held.Release() }()

	g := lock.NewGuarder(dir, 0)
	called := false
	next := func(_ context.Context, _ mcptypes.CallToolRequest) (*mcptypes.CallToolResult, error) {
		called = true
		return mcptypes.NewToolResultText("ok"), nil
	}

	res, err := lockingMiddleware(g)(next)(context.Background(), makeToolReq(map[string]any{}))
	require.NoError(t, err)
	assert.True(t, res.IsError, "a lock-acquisition failure must be a tool error")
	assert.False(t, called, "the handler must not run when the lock cannot be acquired")
}

// TestMCP_NewServer_WithLocker_InstallsMiddleware is a smoke test that wiring a
// locker does not break server construction (the middleware path is exercised
// by the direct middleware tests above). Refs: MGIT-46
func TestMCP_NewServer_WithLocker_InstallsMiddleware(t *testing.T) {
	tmpDir := t.TempDir()
	clock := fixedClock()
	repo, err := gitstore.Init(tmpDir, clock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = repo.Close() })
	idx, err := index.New(filepath.Join(tmpDir, ".mgit", "index.db"), clock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = idx.Close() })

	g := lock.NewGuarder(filepath.Join(tmpDir, ".mgit"), lock.DefaultTimeout)
	srv := NewServer(repo, idx, WithLocker(g))
	require.NotNil(t, srv)
	require.NotNil(t, srv.MCPServer())
}
