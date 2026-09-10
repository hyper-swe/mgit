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

// The socket answering must IMPLY the record exists. The daemon used to
// bind and listen first and write its record after, so a client that
// connected the instant the socket answered could read the record's path
// and find nothing — once, on the mac runner, in the v0.6.6 release hook,
// which was enough (MGIT-204). The hook below holds the daemon right after
// its bind, which turns a microsecond window into a deterministic one: on
// the old order this test fails every time, on the new order never.
// Refs: MGIT-204, MGIT-191
func TestDaemon_RecordExistsBeforeTheSocketAnswers(t *testing.T) {
	skipUnsupportedHostIPC(t)
	cfg, _ := testConfig(t, newFakeManager())
	cfg.RepoRoot = t.TempDir()
	listening := make(chan struct{})
	release := make(chan struct{})
	cfg.afterListen = func() {
		close(listening)
		<-release
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := runDaemon(ctx, t, cfg)
	select {
	case <-listening:
	case <-time.After(5 * time.Second):
		t.Fatal("the daemon never bound its socket")
	}
	_, err := daemonrec.Read(filepath.Join(filepath.Dir(cfg.SocketPath), daemonrec.FileName))
	close(release)
	require.NoError(t, err, "the record must exist by the time the socket is bound")
	cancel()
	require.NoError(t, <-done)
	_, err = os.Stat(filepath.Join(filepath.Dir(cfg.SocketPath), daemonrec.FileName))
	assert.True(t, os.IsNotExist(err), "the record must not outlive the daemon")
}
