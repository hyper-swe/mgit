package git

import (
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestWriteSyncStatePreservesPublishedStateAfterShortWrite(t *testing.T) {
	const childRoot = "MGIT_TEST_SHORT_SYNC_WRITE_ROOT"
	if root := os.Getenv(childRoot); root != "" {
		repo := &Repository{root: root}
		// Limit regular file writes in this child only, never in the test runner.
		var lim unix.Rlimit
		require.NoError(t, unix.Getrlimit(unix.RLIMIT_FSIZE, &lim))
		require.GreaterOrEqual(t, lim.Cur, uint64(128))
		original := lim
		lim.Cur = 128
		signal.Ignore(unix.SIGXFSZ)
		require.NoError(t, unix.Setrlimit(unix.RLIMIT_FSIZE, &lim))
		t.Cleanup(func() {
			require.NoError(t, unix.Setrlimit(unix.RLIMIT_FSIZE, &original))
			signal.Reset(unix.SIGXFSZ)
		})
		err := repo.WriteSyncState(SyncState{WorkTreeHash: strings.Repeat("x", 512)})
		require.ErrorContains(t, err, "write temp sync state")
		require.ErrorIs(t, err, unix.EFBIG)
		return
	}

	repo := &Repository{root: t.TempDir()}
	old := SyncState{GitHead: "previous", BaseCommit: "saved"}
	err := repo.WriteSyncState(old)
	require.ErrorContains(t, err, "create temp sync state")
	require.ErrorIs(t, err, os.ErrNotExist)
	require.NoError(t, os.Mkdir(repo.MgitDir(), 0o700))
	require.NoError(t, repo.WriteSyncState(old))

	bin, err := os.Executable()
	require.NoError(t, err)
	cmd := exec.CommandContext(t.Context(), bin, "-test.run=^TestWriteSyncStatePreservesPublishedStateAfterShortWrite$") //nolint:gosec // fixed test in the current test executable
	cmd.Env = append(os.Environ(), childRoot+"="+repo.Root())
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", output)

	got, found, err := repo.ReadSyncState()
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, old, got)
	left, err := filepath.Glob(filepath.Join(repo.MgitDir(), syncStateName+".tmp-*"))
	require.NoError(t, err)
	require.Empty(t, left)
}
