package sandboxd

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/sandboxd/daemonrec"
)

// A daemon whose repository root has been deleted drains itself within one
// idle-check interval: four of them outlived their mktemp roots by eleven
// days on this host with nothing to notice. Refs: MGIT-191, MGIT-185
func TestDaemon_RepoRootVanished_DrainsItself(t *testing.T) {
	skipUnsupportedHostIPC(t)
	cfg, logs := testConfig(t, newFakeManager())
	root := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, os.MkdirAll(root, 0o700))
	cfg.RepoRoot = root
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := runDaemon(ctx, t, cfg)
	_ = waitForSocket(t, cfg.SocketPath).Close()

	require.NoError(t, os.RemoveAll(root))

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("the daemon kept serving a repository that no longer exists")
	}
	assert.Contains(t, logs.String(), `"event":"repo_root_vanished"`)
	assert.Contains(t, logs.String(), root)
}

// While it runs, a daemon is findable host-wide: its record sits beside its
// socket with its pid and root, and goes away with it. Refs: MGIT-191
func TestDaemon_WritesItsRecordBesideTheSocket_AndRemovesItOnExit(t *testing.T) {
	skipUnsupportedHostIPC(t)
	cfg, _ := testConfig(t, newFakeManager())
	cfg.RepoRoot = t.TempDir()
	cfg.Version = "test-build"
	ctx, cancel := context.WithCancel(context.Background())
	done := runDaemon(ctx, t, cfg)
	_ = waitForSocket(t, cfg.SocketPath).Close()

	rec, err := daemonrec.Read(filepath.Join(filepath.Dir(cfg.SocketPath), daemonrec.FileName))
	require.NoError(t, err)
	assert.Equal(t, os.Getpid(), rec.PID)
	assert.Equal(t, cfg.RepoRoot, rec.RepoRoot)
	assert.Equal(t, cfg.SocketPath, rec.Socket)
	assert.Equal(t, "test-build", rec.Version)
	assert.False(t, rec.StartedAt.IsZero())

	cancel()
	require.NoError(t, <-done)
	_, err = os.Stat(filepath.Join(filepath.Dir(cfg.SocketPath), daemonrec.FileName))
	assert.True(t, os.IsNotExist(err), "the record must not outlive the daemon")
}
