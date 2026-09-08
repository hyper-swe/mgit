package sandboxd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Two daemons served one repository's sandbox state — keyed on two spellings
// of its path — and the second one's rehydration deleted the first one's live
// sandbox with a `killed` event that never happened (MGIT-197). The host root
// that holds the index is the fact's owner: a daemon claims it exclusively
// before it serves, and a second daemon for the same host root refuses to
// start, naming the holder, while the first keeps serving. Refs: MGIT-197
func TestDaemon_SecondDaemonForOneHostRoot_RefusedNamingTheFirst(t *testing.T) {
	skipUnsupportedHostIPC(t)
	hostRoot := t.TempDir()
	cfgA, _ := testConfig(t, newFakeManager())
	cfgA.HostRoot = hostRoot
	cfgB, logsB := testConfig(t, newFakeManager())
	cfgB.HostRoot = hostRoot
	require.NotEqual(t, cfgA.SocketPath, cfgB.SocketPath)

	ctxA, cancelA := context.WithCancel(context.Background())
	doneA := runDaemon(ctxA, t, cfgA)
	_ = waitForSocket(t, cfgA.SocketPath).Close()

	ctxB, cancelB := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelB()
	var errB error
	select {
	case errB = <-runDaemon(ctxB, t, cfgB):
	case <-time.After(2 * time.Second):
		t.Fatal("the second daemon kept running over a host root another daemon already serves")
	}
	require.Error(t, errB)
	for _, want := range []string{"already serves", hostRoot, cfgA.SocketPath, fmt.Sprintf("pid %d", os.Getpid())} {
		assert.Contains(t, errB.Error(), want)
	}
	_, statErr := os.Stat(cfgB.SocketPath)
	assert.True(t, os.IsNotExist(statErr), "the refused daemon must not leave a socket behind")
	assert.NotContains(t, logsB.String(), `"event":"started"`, "a refused daemon never announces itself")

	// The first daemon is untouched by the refusal.
	_ = waitForSocket(t, cfgA.SocketPath).Close()

	// Once the holder exits, the lock is free and the same config starts.
	cancelA()
	require.NoError(t, <-doneA)
	ctxC, cancelC := context.WithCancel(context.Background())
	defer cancelC()
	cfgC, _ := testConfig(t, newFakeManager())
	cfgC.HostRoot = hostRoot
	doneC := runDaemon(ctxC, t, cfgC)
	_ = waitForSocket(t, cfgC.SocketPath).Close()
	cancelC()
	require.NoError(t, <-doneC)
	_, err := os.Stat(filepath.Join(hostRoot, "daemon.lock"))
	assert.NoError(t, err, "the lock file stays; only the claim on it is released")
}

// The claim is taken before the wiring that used to create the host root, so
// on a repository's very first daemon start the directory does not exist yet:
// the firecracker LIVE leg's fresh repository failed with "open lock …: no
// such file or directory" and no daemon could ever start there. The claim
// creates the directory it claims. Refs: MGIT-197
func TestClaimHostRoot_CreatesAnAbsentHostRoot(t *testing.T) {
	hostRoot := filepath.Join(t.TempDir(), "repo", ".mgit", "sandbox")
	claim, err := ClaimHostRoot(hostRoot, "/s/d.sock")
	require.NoError(t, err)
	defer claim.Release()
	info, err := os.Stat(hostRoot)
	require.NoError(t, err)
	assert.True(t, info.IsDir())
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm(), "the host root is the operator's alone")
	data, err := os.ReadFile(filepath.Join(hostRoot, hostLockName))
	require.NoError(t, err)
	assert.Contains(t, string(data), "socket /s/d.sock")
}
