package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeAPI stands in for the REST server: Start blocks until Shutdown (like
// echo) unless startErr is set, in which case it fails immediately.
type fakeAPI struct {
	mu             sync.Mutex
	shutdownCalled bool
	startErr       error
	release        chan struct{}
}

func newFakeAPI() *fakeAPI { return &fakeAPI{release: make(chan struct{})} }

func (f *fakeAPI) Start(string) error {
	if f.startErr != nil {
		return f.startErr
	}
	<-f.release
	return http.ErrServerClosed
}

func (f *fakeAPI) Shutdown(context.Context) error {
	f.mu.Lock()
	f.shutdownCalled = true
	f.mu.Unlock()
	select {
	case <-f.release:
	default:
		close(f.release)
	}
	return nil
}

func (f *fakeAPI) didShutdown() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.shutdownCalled
}

// fakeMCP serves until the context is canceled.
type fakeMCP struct{}

func (fakeMCP) Serve(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }

// TestServe_Command_Help verifies the command documents its flags.
func TestServe_Command_Help(t *testing.T) {
	cmd := serveCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--help"})
	require.NoError(t, cmd.Execute())
	for _, flag := range []string{"port", "api-only", "mcp-only", "json"} {
		assert.Contains(t, out.String(), flag, "help documents --%s", flag)
	}
}

// TestServe_DefaultPort verifies the default REST port is 6860 (FR-9.1).
func TestServe_DefaultPort(t *testing.T) {
	assert.Equal(t, "6860", serveCmd().Flags().Lookup("port").DefValue)
}

// TestServe_APIOnly_Flag verifies --api-only selects only the REST API and
// the flag is registered.
func TestServe_APIOnly_Flag(t *testing.T) {
	require.NotNil(t, serveCmd().Flags().Lookup("api-only"))
	api, mcp, err := resolveServeMode(true, false)
	require.NoError(t, err)
	assert.True(t, api)
	assert.False(t, mcp)
}

// TestServe_MCPOnly_Flag verifies --mcp-only selects only the MCP server.
func TestServe_MCPOnly_Flag(t *testing.T) {
	require.NotNil(t, serveCmd().Flags().Lookup("mcp-only"))
	api, mcp, err := resolveServeMode(false, true)
	require.NoError(t, err)
	assert.False(t, api)
	assert.True(t, mcp)
}

// TestServe_BothModes_Exclusive verifies the two restricting flags conflict.
func TestServe_BothModes_Exclusive(t *testing.T) {
	_, _, err := resolveServeMode(true, true)
	assert.Error(t, err)
}

// TestServe_DefaultBoth verifies neither flag runs both servers.
func TestServe_DefaultBoth(t *testing.T) {
	api, mcp, err := resolveServeMode(false, false)
	require.NoError(t, err)
	assert.True(t, api)
	assert.True(t, mcp)
}

// TestServe_LocalhostBinding verifies the REST API binds to localhost only
// (never 0.0.0.0). Refs: NFR-5
func TestServe_LocalhostBinding(t *testing.T) {
	assert.Equal(t, "127.0.0.1:6860", apiAddr(6860))
}

// TestRunServe_GracefulShutdown verifies a canceled context (signal) stops
// the API cleanly and returns no error.
func TestRunServe_GracefulShutdown(t *testing.T) {
	api := newFakeAPI()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // simulate SIGINT
	err := runServe(ctx, api, fakeMCP{}, serveOptions{port: 6860, startAPI: true, startMCP: true}, io.Discard)
	require.NoError(t, err)
	assert.True(t, api.didShutdown(), "the REST server is shut down gracefully")
}

// TestRunServe_ServerError_Surfaces verifies a fatal server error surfaces
// and the API is still shut down.
func TestRunServe_ServerError_Surfaces(t *testing.T) {
	api := &fakeAPI{startErr: errors.New("bind failed"), release: make(chan struct{})}
	err := runServe(context.Background(), api, fakeMCP{}, serveOptions{port: 6860, startAPI: true}, io.Discard)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rest api")
	assert.True(t, api.didShutdown())
}

// TestRunServe_MCPOnly_NoAPIShutdown verifies MCP-only mode returns cleanly
// on cancel without touching the (absent) API.
func TestRunServe_MCPOnly_NoAPIShutdown(t *testing.T) {
	api := newFakeAPI()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := runServe(ctx, api, fakeMCP{}, serveOptions{startAPI: false, startMCP: true}, io.Discard)
	require.NoError(t, err)
	assert.False(t, api.didShutdown(), "no API was started, so none is shut down")
}

// TestAnnounceServe_JSONToStderr verifies startup info is structured and
// written to the provided (stderr) writer, not stdout.
func TestAnnounceServe_JSON(t *testing.T) {
	var buf bytes.Buffer
	announceServe(&buf, serveOptions{port: 6860, startAPI: true, startMCP: true, asJSON: true}, apiAddr(6860))
	assert.Contains(t, buf.String(), `"mode":"api+mcp"`)
	assert.Contains(t, buf.String(), `"api_addr":"127.0.0.1:6860"`)
}

// TestServeModeLabel covers the three mode labels.
func TestServeModeLabel(t *testing.T) {
	assert.Equal(t, "api+mcp", serveModeLabel(serveOptions{startAPI: true, startMCP: true}))
	assert.Equal(t, "api", serveModeLabel(serveOptions{startAPI: true}))
	assert.Equal(t, "mcp", serveModeLabel(serveOptions{startMCP: true}))
}
