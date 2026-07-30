package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A DAEMON THAT DIES MUST SAY WHY.
//
// Activation failure reached the user as
//
//	sandbox daemon unavailable …: /var/folders/…/d.sock not dialable after spawn
//
// for EVERY cause: a missing libkrun, a directory that did not exist, a
// corrupt database. The daemon printed a perfectly good explanation, but it
// went to a detached session nobody was reading. On a Mac without libkrun —
// the single most likely first-run failure — the real cause is a dyld error
// the process emits before main() even runs, so no amount of in-daemon
// capability checking can surface it. It has to be captured and read back
// here. Refs: MGIT-61.14, MGIT-61.15, NFR-17.6

func TestDaemonFailureDetail_NamesTheMissingLibraryAndHowToGetIt(t *testing.T) {
	tests := []struct {
		name    string
		log     string
		want    []string
		notWant []string
	}{
		{
			// Verbatim from a real archive on a Mac with libkrun absent.
			name: "macos_dyld",
			log: "dyld[95534]: Library not loaded: /opt/homebrew/opt/libkrun/lib/libkrun.1.dylib\n" +
				"  Referenced from: <1D9BC4F8> /usr/local/bin/mgit-sandboxd\n" +
				"  Reason: tried: '/opt/homebrew/opt/libkrun/lib/libkrun.1.dylib' (no such file)\n",
			want: []string{"libkrun", "brew install", "INSTALL-SANDBOX"},
		},
		{
			name: "linux_ld_so",
			log: "mgit-sandboxd: error while loading shared libraries: libkrun.so.1: " +
				"cannot open shared object file: No such file or directory\n",
			want: []string{"libkrun", "INSTALL-SANDBOX"},
		},
		{
			// Some other library entirely: still worth naming, but we must
			// not claim brew installs it — we do not know that it does.
			name: "an_unrelated_library",
			log: "dyld[1]: Library not loaded: /usr/local/lib/libsomething.3.dylib\n" +
				"  Reason: tried: '/usr/local/lib/libsomething.3.dylib' (no such file)\n",
			want:    []string{"libsomething.3.dylib", "INSTALL-SANDBOX"},
			notWant: []string{"brew"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := daemonFailureDetail(writeDaemonLog(t, tt.log))
			for _, want := range tt.want {
				assert.Contains(t, got, want, "the remedy must be actionable, got %q", got)
			}
			for _, notWant := range tt.notWant {
				assert.NotContains(t, got, notWant,
					"a remedy must not be invented for a cause it does not fit")
			}
		})
	}
}

func TestDaemonFailureDetail_ReadsTheDaemonsOwnLogFormat(t *testing.T) {
	// Not every failure has a remedy we can name. What every failure has is
	// the daemon's own message — but the daemon logs JSON, and handing a user
	// a raw slog record to read is barely better than not telling them.
	got := daemonFailureDetail(writeDaemonLog(t,
		`{"time":"2026-07-30T16:28:58Z","level":"INFO","msg":"sandbox VMM linked at build time"}`+"\n"+
			`{"time":"2026-07-30T16:28:58Z","level":"ERROR","msg":"sandbox service wiring failed",`+
			`"error":"open sandbox audit index: unable to open database file"}`+"\n"))

	assert.Contains(t, got, "sandbox service wiring failed")
	assert.Contains(t, got, "open sandbox audit index",
		"the error field carries the actual cause and must survive")
	assert.NotContains(t, got, `"level"`, "the user should not have to read JSON")
	assert.NotContains(t, got, "brew install",
		"a remedy must not be invented for a cause it does not fit")
}

func TestDaemonFailureDetail_UnparseableOutputSurvivesVerbatim(t *testing.T) {
	// The case that matters most is not JSON at all: the dynamic loader
	// writes plain text, and losing it to a failed decode would lose the one
	// failure we most need to explain.
	got := daemonFailureDetail(writeDaemonLog(t, "panic: something went very wrong\n\tmain.go:1\n"))

	assert.Contains(t, got, "panic: something went very wrong")
}

func TestDaemonFailureDetail_SaysNothingWhenItKnowsNothing(t *testing.T) {
	// A daemon that failed before writing anything, or a log we cannot read,
	// must add no noise to the error the user already has.
	assert.Empty(t, daemonFailureDetail(filepath.Join(t.TempDir(), "absent.log")))
	assert.Empty(t, daemonFailureDetail(writeDaemonLog(t, "   \n\n")))
}

func TestDaemonFailureDetail_KeepsTheTailOfANoisyLog(t *testing.T) {
	// The daemon logs its startup progress before failing. The failure is at
	// the END, and an error message is not the place for the whole file.
	var b strings.Builder
	for i := range 200 {
		b.WriteString("noise line ")
		b.WriteString(string(rune('a' + i%26)))
		b.WriteString("\n")
	}
	b.WriteString("the actual cause\n")

	got := daemonFailureDetail(writeDaemonLog(t, b.String()))

	assert.Contains(t, got, "the actual cause")
	assert.Less(t, len(got), 2000, "the detail must stay readable, got %d bytes", len(got))
}

// writeDaemonLog writes a daemon log fixture and returns its path.
func writeDaemonLog(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "daemon.log")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

// TestLocateSandboxd_FollowsASymlinkedInstall is the daemon-side twin of the
// guest-binary lookup.
//
// Extract the archive, symlink mgit onto PATH, and macOS reports the SYMLINK
// as the executable — so "beside my own binary" resolves to the symlink's
// directory, which holds no daemon. The two binaries ship together precisely
// so they can find each other; that must survive the ordinary way people put
// a binary on PATH. Refs: MGIT-65, MGIT-44
func TestLocateSandboxd_FollowsASymlinkedInstall(t *testing.T) {
	install := t.TempDir()
	for _, n := range []string{"mgit", "mgit-sandboxd"} {
		require.NoError(t, os.WriteFile(filepath.Join(install, n), []byte("x"), 0o600))
	}
	onPath := filepath.Join(t.TempDir(), "mgit")
	require.NoError(t, os.Symlink(filepath.Join(install, "mgit"), onPath))

	got, err := locateSandboxdFor(onPath)

	require.NoError(t, err, "the daemon sits beside the real binary and must be found")
	resolved, err := filepath.EvalSymlinks(filepath.Join(install, "mgit-sandboxd"))
	require.NoError(t, err)
	assert.Equal(t, resolved, got)
}
