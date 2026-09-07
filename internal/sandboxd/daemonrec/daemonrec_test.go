package daemonrec

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var t0 = time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)

// A daemon's record lives beside its socket, says who it is, and is gone
// when the daemon is. Refs: MGIT-191, MGIT-185
func TestRecord_WriteReadRemove_BesideTheSocket(t *testing.T) {
	dir := t.TempDir()
	rec := Record{PID: 4242, RepoRoot: "/work/repo", HostRoot: "/work/repo/.mgit/sandbox",
		Socket: filepath.Join(dir, "d.sock"), StartedAt: t0, Version: "0.6.6 (commit: abc)"}

	require.NoError(t, Write(rec))
	got, err := Read(filepath.Join(dir, FileName))
	require.NoError(t, err)
	assert.Equal(t, rec, got)

	require.NoError(t, Remove(rec.Socket))
	_, err = os.Stat(filepath.Join(dir, FileName))
	assert.True(t, os.IsNotExist(err), "the record is removed with the daemon")
	assert.NoError(t, Remove(rec.Socket), "removing twice is not an error")
}

// List walks one runtime base: every record under it, in a stable order, with
// unreadable or malformed records reported rather than skipped silently.
func TestList_WalksTheRuntimeBase_AndNamesWhatItCannotRead(t *testing.T) {
	base := t.TempDir()
	for i, key := range []string{"aaa", "bbb"} {
		dir := filepath.Join(base, key)
		require.NoError(t, os.MkdirAll(dir, 0o700))
		require.NoError(t, Write(Record{PID: 100 + i, RepoRoot: "/r/" + key, Socket: filepath.Join(dir, "d.sock"), StartedAt: t0}))
	}
	require.NoError(t, os.MkdirAll(filepath.Join(base, "ccc"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(base, "ccc", FileName), []byte("{not json"), 0o600))

	recs, problems, err := List(base)

	require.NoError(t, err)
	require.Len(t, recs, 2)
	assert.Equal(t, "/r/aaa", recs[0].RepoRoot)
	assert.Equal(t, "/r/bbb", recs[1].RepoRoot)
	require.Len(t, problems, 1)
	assert.Contains(t, problems[0], "ccc")
}

// Classify says what an operator needs to know about each daemon: whether
// its process is alive, whether its repository root still exists, whether
// that root is a temp directory, and how long it has run. The rows are the
// shapes MGIT-185 and MGIT-191 found on this host, not anything derived
// from the classifier. Refs: MGIT-191
func TestClassify_FlagsTheShapesTheHostShowed(t *testing.T) {
	// t.TempDir() is itself under the OS temp dir, so the classifier's notion
	// of "temp root" is pinned to a directory of the test's own choosing.
	root := t.TempDir()
	gone := filepath.Join(root, "gone")
	scratch := filepath.Join(t.TempDir(), "scratch")
	tmpRoot := filepath.Join(scratch, "tmp.abc123")
	alive := func(pid int) bool { return pid != 9999 }
	tests := []struct {
		name string
		rec  Record
		want Status
	}{
		{"healthy_repo_daemon", Record{PID: 1, RepoRoot: root, StartedAt: t0.Add(-time.Hour)},
			Status{Alive: true, Age: time.Hour}},
		{"root_vanished_must_have_drained", Record{PID: 2, RepoRoot: gone, StartedAt: t0.Add(-time.Hour)},
			Status{Alive: true, RootGone: true, Age: time.Hour}},
		{"temp_root_eleven_days_is_the_MGIT_191_shape", Record{PID: 3, RepoRoot: tmpRoot, StartedAt: t0.Add(-11 * 24 * time.Hour)},
			Status{Alive: true, TempRoot: true, RootGone: true, Age: 11 * 24 * time.Hour}},
		{"dead_pid_is_a_stale_record", Record{PID: 9999, RepoRoot: root, StartedAt: t0.Add(-time.Minute)},
			Status{Alive: false, Age: time.Minute}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Classify(tt.rec, t0, alive, []string{scratch})
			assert.Equal(t, tt.want, got)
		})
	}
}

// A leak is a daemon whose root is gone, or a temp-root daemon older than
// the grace an e2e run could plausibly need. Refs: MGIT-191
func TestStatus_Leaked(t *testing.T) {
	assert.False(t, Status{Alive: true, Age: time.Hour}.Leaked(24*time.Hour))
	assert.True(t, Status{Alive: true, RootGone: true}.Leaked(24*time.Hour))
	assert.False(t, Status{Alive: true, TempRoot: true, Age: time.Hour}.Leaked(24*time.Hour), "a fresh e2e daemon is not a leak")
	assert.True(t, Status{Alive: true, TempRoot: true, Age: 2 * 24 * time.Hour}.Leaked(24*time.Hour))
	assert.False(t, Status{Alive: false, RootGone: true}.Leaked(24*time.Hour), "a dead daemon leaks nothing; its record is stale")
}
