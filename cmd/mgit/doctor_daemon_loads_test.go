package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeDaemon writes an executable that behaves like mgit-sandboxd would on
// the host state under test and returns its path.
func fakeDaemon(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "mgit-sandboxd")
	require.NoError(t, os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755)) //nolint:gosec // a test executable
	return p
}

// The probe is the doctor row's only witness, so it is exercised against
// the exact loader lines MGIT-206 and MGIT-61.15 saw: macOS's dyld refusal,
// glibc's, a daemon that answers, and one that hangs (bounded by the
// deadline, never by the reader's patience).
//
// Only the hanging case carries a caller's deadline. The cases whose subject
// is the ANSWER run under the probe's own bound (daemonLoadProbeTimeout, the
// production one): a 2 s deadline shared by every case turned the v0.6.7
// pre-tag dry run red on a loaded host, where the fake daemon's shell had not
// printed in time and the probe rightly reported no answer — a timing failure
// inside a parsing test, in the same release hook that burned v0.6.6. The
// paused case pins it: it answers later than that old deadline and must
// still be read. Refs: MGIT-210, MGIT-206
func TestProbeDaemonLoadsAt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake daemons are shell scripts")
	}
	const version = "mgit-sandboxd version 0.6.6 (commit: abc, built: now)"
	tests := []struct {
		name        string
		body        string
		deadline    time.Duration // the caller's; 0 leaves the probe's own bound
		wantVersion string
		wantLib     string
		wantOut     string
		wantRemedy  string
	}{
		{"answers_version", `echo "` + version + `"`, 0, version, "", "", ""},
		{"answers_after_a_pause", `sleep 3; echo "` + version + `"`, 0, version, "", "", ""},
		{"dyld_refuses", `echo "dyld[93889]: Library not loaded: /opt/homebrew/opt/virglrenderer/lib/libvirglrenderer.1.dylib" >&2; exit 1`,
			0, "", "libvirglrenderer.1.dylib", "Library not loaded", "libvirglrenderer.1.dylib is missing"},
		{"glibc_refuses", `echo "mgit-sandboxd: error while loading shared libraries: libkrun.so.1: cannot open shared object file" >&2; exit 127`,
			0, "", "libkrun.so.1", "libkrun.so.1", "brew tap libkrun/krun"},
		{"dies_for_another_reason", `echo "panic: something else" >&2; exit 2`,
			0, "", "", "panic: something else", ""},
		{"hangs_is_bounded", `sleep 30`, 2 * time.Second, "", "", "no answer within", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			if tt.deadline > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tt.deadline)
				defer cancel()
			}
			start := time.Now()
			got, err := probeDaemonLoadsAt(ctx, fakeDaemon(t, tt.body))
			require.NoError(t, err, "a binary that exists is always probed; only a missing one is an error")
			if tt.deadline > 0 {
				assert.Less(t, time.Since(start), tt.deadline+5*time.Second, "bounded by the deadline, not by the daemon")
			}
			assert.Equal(t, tt.wantVersion, got.Version)
			assert.Equal(t, tt.wantLib, got.MissingLibrary)
			assert.Contains(t, got.Output, tt.wantOut)
			assert.Contains(t, got.Remedy, tt.wantRemedy)
			if tt.wantLib == "" {
				assert.Empty(t, got.Remedy, "no remedy is invented for a library nobody named")
			}
		})
	}
}

// A core-only install has no daemon beside mgit and none on PATH: the probe
// reports that as an error, which the row turns into not-checked.
func TestProbeDaemonLoadsAt_NoBinary_Errors(t *testing.T) {
	_, err := probeDaemonLoadsAt(context.Background(), filepath.Join(t.TempDir(), "absent"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "absent")
}
