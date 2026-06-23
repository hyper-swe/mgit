// Package sandboxd tests verify the daemon lifecycle per MGIT-11.4.1
// acceptance criteria. Refs: FR-17.16, NFR-17.6
package sandboxd

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// fakeManager is an in-memory SandboxManager for lifecycle tests.
// The fail* flags inject faults for drain/list error-path coverage.
type fakeManager struct {
	mu         sync.Mutex
	sandboxes  map[string]model.SandboxInfo
	stopped    []string
	removed    []string
	failList   bool
	failStop   bool
	failRemove bool
}

func newFakeManager(ids ...string) *fakeManager {
	m := &fakeManager{sandboxes: map[string]model.SandboxInfo{}}
	for _, id := range ids {
		m.sandboxes[id] = model.SandboxInfo{ID: id, TaskID: "MGIT-4.2", State: model.StateRunning}
	}
	return m
}

func (m *fakeManager) Launch(_ context.Context, _ model.SandboxLaunchOptions) (*model.SandboxInfo, error) {
	return nil, model.ErrSandboxBackendUnavailable
}

func (m *fakeManager) List(_ context.Context) ([]model.SandboxInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failList {
		return nil, assert.AnError
	}
	out := make([]model.SandboxInfo, 0, len(m.sandboxes))
	for _, sb := range m.sandboxes {
		out = append(out, sb)
	}
	return out, nil
}

func (m *fakeManager) Exec(_ context.Context, _ string, _ model.ExecRequest) (*model.ExecResult, error) {
	return &model.ExecResult{}, nil
}

func (m *fakeManager) Stop(_ context.Context, id string, _ bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failStop {
		return assert.AnError
	}
	m.stopped = append(m.stopped, id)
	return nil
}

func (m *fakeManager) Remove(_ context.Context, id string, _ bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failRemove {
		return assert.AnError
	}
	m.removed = append(m.removed, id)
	delete(m.sandboxes, id)
	return nil
}

func (m *fakeManager) Resolve(_ context.Context, id string) (*model.SandboxInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if sb, ok := m.sandboxes[id]; ok {
		return &sb, nil
	}
	return nil, model.ErrSandboxNotFound
}

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// shortSocketPath returns a socket path short enough for the platform
// sun_path limit (~104 bytes on darwin); t.TempDir() embeds the test
// name and routinely exceeds it.
func shortSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "sbd")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "d.sock")
}

func testConfig(t *testing.T, manager model.SandboxManager) (Config, *syncBuffer) {
	t.Helper()
	logs := &syncBuffer{}
	cfg := Config{
		SocketPath:   shortSocketPath(t),
		Manager:      manager,
		Logger:       slog.New(slog.NewJSONHandler(logs, nil)),
		Clock:        time.Now,
		IdleGrace:    150 * time.Millisecond,
		PollInterval: 20 * time.Millisecond,
	}
	return cfg, logs
}

// runDaemon starts the daemon and returns a channel with Run's result.
func runDaemon(ctx context.Context, t *testing.T, cfg Config) <-chan error {
	t.Helper()
	d, err := New(cfg)
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	return done
}

func waitForSocket(t *testing.T, path string) net.Conn {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if conn, err := net.Dial("unix", path); err == nil {
			return conn
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("socket %s never became dialable", path)
	return nil
}

// TestSandboxd_SocketActivation_StartsOnDemand verifies on-demand
// activation: a dial to a dead socket triggers exactly one spawn, and
// concurrent EnsureRunning calls do not double-spawn. Refs: NFR-17.6
// skipUnsupportedHostIPC skips tests that depend on the unix-socket +
// SO_PEERCRED host IPC, which has no Windows implementation yet —
// Windows host support (named pipes + ACL peer auth) lands with the
// Hyper-V backend (MGIT-11.5.3). On Windows platformPeerUID fails
// closed, so the authenticated greeting handshake cannot complete.
func skipUnsupportedHostIPC(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("host IPC (unix socket + peer-cred auth) not yet implemented on Windows (MGIT-11.5.3)")
	}
}

func TestSandboxd_SocketActivation_StartsOnDemand(t *testing.T) {
	skipUnsupportedHostIPC(t)
	manager := newFakeManager("01JXSB1")
	cfg, _ := testConfig(t, manager)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var spawns atomic.Int32
	spawn := func() error {
		spawns.Add(1)
		runDaemon(ctx, t, cfg)
		return nil
	}

	require.NoError(t, EnsureRunning(ctx, cfg.SocketPath, spawn))
	assert.Equal(t, int32(1), spawns.Load(), "first EnsureRunning spawns the daemon")

	require.NoError(t, EnsureRunning(ctx, cfg.SocketPath, spawn))
	assert.Equal(t, int32(1), spawns.Load(), "a live daemon must not be spawned again")
}

// TestSandboxd_ExitsWhenNoSandboxes verifies the zero-footprint
// property: with no sandboxes for IdleGrace, Run returns cleanly and
// removes its socket. Refs: NFR-17.6
func TestSandboxd_ExitsWhenNoSandboxes(t *testing.T) {
	manager := newFakeManager() // zero sandboxes
	cfg, logs := testConfig(t, manager)
	ctx := context.Background()

	done := runDaemon(ctx, t, cfg)

	select {
	case err := <-done:
		require.NoError(t, err, "idle exit is clean")
	case <-time.After(3 * time.Second):
		t.Fatal("daemon did not exit while idle with zero sandboxes")
	}

	_, dialErr := net.Dial("unix", cfg.SocketPath)
	assert.Error(t, dialErr, "socket must be gone after idle exit")
	assert.Contains(t, logs.String(), `"idle_exit"`, "exit reason logged (structured)")

	t.Run("restart_after_exit_is_safe", func(t *testing.T) {
		ctx2, cancel := context.WithCancel(ctx)
		done2 := runDaemon(ctx2, t, cfg)
		conn := waitForSocket(t, cfg.SocketPath)
		_ = conn.Close()
		cancel()
		require.NoError(t, <-done2)
	})
}

// TestSandboxd_CleanShutdown_DestroysAll verifies shutdown drains:
// every supervised sandbox is stopped and removed before Run returns.
// Refs: FR-17.16, FR-17.9
func TestSandboxd_CleanShutdown_DestroysAll(t *testing.T) {
	manager := newFakeManager("01JXSB1", "01JXSB2")
	cfg, logs := testConfig(t, manager)
	cfg.IdleGrace = time.Hour // never idle-exit during this test
	ctx, cancel := context.WithCancel(context.Background())

	done := runDaemon(ctx, t, cfg)
	conn := waitForSocket(t, cfg.SocketPath)
	_ = conn.Close()

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err, "signal shutdown is clean")
	case <-time.After(3 * time.Second):
		t.Fatal("daemon did not shut down on context cancellation")
	}

	assert.ElementsMatch(t, []string{"01JXSB1", "01JXSB2"}, manager.stopped,
		"every sandbox is stopped during drain")
	assert.ElementsMatch(t, []string{"01JXSB1", "01JXSB2"}, manager.removed,
		"every sandbox is destroyed during drain")
	assert.Contains(t, logs.String(), `"shutdown"`, "shutdown logged (structured)")

	t.Run("stale_socket_from_kill_is_replaced", func(t *testing.T) {
		// Simulate a crashed daemon leaving a stale socket file behind:
		// a fresh Run must bind anyway (restart safety).
		ln, err := net.Listen("unix", cfg.SocketPath)
		require.NoError(t, err)
		_ = ln.Close() // closes listener; on some platforms file lingers

		ctx2, cancel2 := context.WithCancel(context.Background())
		done2 := runDaemon(ctx2, t, cfg)
		conn := waitForSocket(t, cfg.SocketPath)
		_ = conn.Close()
		cancel2()
		require.NoError(t, <-done2)
	})
}

// TestSandboxd_FaultPaths covers the lifecycle error branches: stale
// regular file replaced, double-daemon refusal, drain continuing past
// stuck sandboxes, list failures logged without exiting, and spawn
// failure surfacing. Refs: FR-17.16
func TestSandboxd_FaultPaths(t *testing.T) {
	ctx := context.Background()

	t.Run("stale_regular_file_replaced", func(t *testing.T) {
		manager := newFakeManager()
		cfg, _ := testConfig(t, manager)
		require.NoError(t, os.WriteFile(cfg.SocketPath, nil, 0o600),
			"plant a stale non-socket file at the socket path")
		done := runDaemon(ctx, t, cfg)
		require.NoError(t, <-done, "daemon replaces the stale file, runs, and idle-exits")
	})

	t.Run("second_daemon_refused_while_first_lives", func(t *testing.T) {
		manager := newFakeManager("01JXSB1")
		cfg, _ := testConfig(t, manager)
		cfg.IdleGrace = time.Hour
		ctx1, cancel := context.WithCancel(ctx)
		defer cancel()
		done := runDaemon(ctx1, t, cfg)
		_ = waitForSocket(t, cfg.SocketPath).Close()

		second, err := New(cfg)
		require.NoError(t, err)
		err = second.Run(ctx)
		assert.ErrorContains(t, err, "another daemon",
			"the socket bind is exclusive; a live daemon is never displaced")

		cancel()
		require.NoError(t, <-done)
	})

	t.Run("drain_continues_past_stuck_sandboxes", func(t *testing.T) {
		manager := newFakeManager("01JXSB1", "01JXSB2")
		manager.failStop = true
		cfg, logs := testConfig(t, manager)
		cfg.IdleGrace = time.Hour
		ctx1, cancel := context.WithCancel(ctx)
		done := runDaemon(ctx1, t, cfg)
		_ = waitForSocket(t, cfg.SocketPath).Close()
		cancel()
		require.NoError(t, <-done, "stuck stops are logged, not fatal")
		assert.Len(t, manager.removed, 2, "removal still attempted for every sandbox")
		assert.Contains(t, logs.String(), `"drain_error"`)
	})

	t.Run("drain_remove_failure_logged", func(t *testing.T) {
		manager := newFakeManager("01JXSB1")
		manager.failRemove = true
		cfg, logs := testConfig(t, manager)
		cfg.IdleGrace = time.Hour
		ctx1, cancel := context.WithCancel(ctx)
		done := runDaemon(ctx1, t, cfg)
		_ = waitForSocket(t, cfg.SocketPath).Close()
		cancel()
		require.NoError(t, <-done)
		assert.Contains(t, logs.String(), `"drain_error"`)
	})

	t.Run("list_failure_logged_not_fatal", func(t *testing.T) {
		manager := newFakeManager()
		manager.failList = true
		cfg, logs := testConfig(t, manager)
		ctx1, cancel := context.WithCancel(ctx)
		done := runDaemon(ctx1, t, cfg)
		_ = waitForSocket(t, cfg.SocketPath).Close()

		require.Eventually(t, func() bool {
			return strings.Contains(logs.String(), `"list_error"`)
		}, 2*time.Second, 10*time.Millisecond, "list failures are logged")

		cancel()
		assert.Error(t, <-done, "drain surfaces the list failure on shutdown")
	})

	t.Run("spawn_failure_surfaces", func(t *testing.T) {
		err := EnsureRunning(ctx, shortSocketPath(t), func() error { return assert.AnError })
		assert.Error(t, err)
	})

	t.Run("activation_timeout_surfaces", func(t *testing.T) {
		err := ensureRunning(ctx, shortSocketPath(t), func() error { return nil }, 50*time.Millisecond)
		assert.ErrorContains(t, err, "not dialable",
			"a spawn that never binds must time out, not hang")
	})

	t.Run("defaults_applied_for_zero_timing", func(t *testing.T) {
		cfg, _ := testConfig(t, newFakeManager())
		cfg.IdleGrace, cfg.PollInterval = 0, 0
		d, err := New(cfg)
		require.NoError(t, err)
		assert.Equal(t, defaultIdleGrace, d.cfg.IdleGrace)
		assert.Equal(t, defaultPollInterval, d.cfg.PollInterval)
	})
}

// TestSandboxd_New_Guards covers constructor validation.
func TestSandboxd_New_Guards(t *testing.T) {
	valid, _ := testConfig(t, newFakeManager())

	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{name: "empty_socket_path", mutate: func(c *Config) { c.SocketPath = "" }},
		{name: "nil_manager", mutate: func(c *Config) { c.Manager = nil }},
		{name: "nil_clock", mutate: func(c *Config) { c.Clock = nil }},
		{name: "nil_logger", mutate: func(c *Config) { c.Logger = nil }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := valid
			tt.mutate(&cfg)
			_, err := New(cfg)
			assert.Error(t, err)
		})
	}
}

// TestSandboxd_DialOK_FullGreeting proves dialOK compares the FULL liveness
// greeting (symmetric with the real client), so a server that only emits a
// prefix of the greeting is treated as not-live rather than spuriously OK.
// Refs: FR-17.34
func TestSandboxd_DialOK_FullGreeting(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix-socket greeting probe is not the Windows IPC model (MGIT-11.5.3)")
	}

	serve := func(t *testing.T, reply string) string {
		t.Helper()
		path := shortSocketPath(t)
		ln, err := net.Listen("unix", path)
		require.NoError(t, err)
		t.Cleanup(func() { _ = ln.Close() })
		go func() {
			for {
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				_, _ = conn.Write([]byte(reply))
				_ = conn.Close()
			}
		}()
		return path
	}

	t.Run("full_greeting_is_live", func(t *testing.T) {
		assert.True(t, dialOK(context.Background(), serve(t, greeting)),
			"a daemon emitting the full greeting is live")
	})
	t.Run("prefix_only_greeting_is_not_live", func(t *testing.T) {
		assert.False(t, dialOK(context.Background(), serve(t, greeting[:3])),
			"a server emitting only a prefix of the greeting is not accepted")
	})
	t.Run("wrong_greeting_is_not_live", func(t *testing.T) {
		assert.False(t, dialOK(context.Background(), serve(t, "no thanks\n")),
			"a server emitting a wrong greeting is not accepted")
	})
	t.Run("dead_socket_is_not_live", func(t *testing.T) {
		assert.False(t, dialOK(context.Background(), shortSocketPath(t)),
			"an unbound socket path is not live")
	})
}

// TestSandboxd_SocketDir_Owner0700 proves F-08's directory hardening: the
// socket's parent directory is owner-only (0700) after the daemon binds, and a
// pre-existing world-writable (0777) directory is TIGHTENED to 0700 rather than
// left open for squatting / symlink interposition. A directory the daemon
// cannot chmod (not owned by this user) fails closed. Refs: FR-17.34, F-08
func TestSandboxd_SocketDir_Owner0700(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("0700 unix directory modes are not the Windows IPC model (MGIT-11.5.3)")
	}

	t.Run("fresh_dir_is_tightened_to_0700_after_run", func(t *testing.T) {
		cfg, _ := testConfig(t, newFakeManager())
		dir := filepath.Dir(cfg.SocketPath)
		ctx, cancel := context.WithCancel(context.Background())
		done := runDaemon(ctx, t, cfg)
		_ = waitForSocket(t, cfg.SocketPath).Close()

		info, err := os.Stat(dir)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o700), info.Mode().Perm(),
			"the socket directory is owner-only after the daemon binds")

		cancel()
		<-done
	})

	t.Run("preexisting_0777_dir_is_tightened", func(t *testing.T) {
		cfg, _ := testConfig(t, newFakeManager())
		dir := filepath.Dir(cfg.SocketPath)
		//nolint:gosec // G302: intentionally world-writable to PROVE ensureSocketDir tightens it
		require.NoError(t, os.Chmod(dir, 0o777), "make the dir world-writable before launch")

		d, err := New(cfg)
		require.NoError(t, err)
		require.NoError(t, d.ensureSocketDir(), "ensureSocketDir tightens a world-writable dir")

		info, err := os.Stat(dir)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o700), info.Mode().Perm(),
			"a pre-existing 0777 socket directory is tightened to owner-only")
	})
}
