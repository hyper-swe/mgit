package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/buildinfo"
)

// TestRun_Version_PrintsTheBuildAndExitsZero pins the exit code, not just the
// output.
//
// The exit code is the whole reason this flag exists. The release checklist's
// Gatekeeper smoke chains `./mgit --version && ./mgit-sandboxd --version` to
// tell "the binary ran" apart from "Gatekeeper SIGKILLed it"; before MGIT-83
// the daemon answered "flag provided but not defined: -version" and exited 2,
// which reads as the failure the step exists to detect. Refs: MGIT-83, MGIT-64
func TestRun_Version_PrintsTheBuildAndExitsZero(t *testing.T) {
	var out, logs bytes.Buffer

	code := run([]string{"--version"}, &out, &logs)

	assert.Equal(t, 0, code, "--version must exit 0; a non-zero exit here is "+
		"indistinguishable from a Gatekeeper kill during the archive smoke test")
	assert.Equal(t, "mgit-sandboxd version "+buildinfo.String(), strings.TrimSpace(out.String()))
}

// TestRun_Version_MatchesMgitsFormat is the point of sharing internal/buildinfo:
// the two binaries ship in ONE archive, so an operator must not have to
// reconcile two version formats. Refs: MGIT-83
func TestRun_Version_MatchesMgitsFormat(t *testing.T) {
	var out, logs bytes.Buffer
	require.Equal(t, 0, run([]string{"--version"}, &out, &logs))

	got := strings.TrimSpace(out.String())
	v, c, d := buildinfo.Resolve()
	assert.Equal(t, "mgit-sandboxd version "+buildinfo.Format(v, c, d), got,
		"the daemon must render the same one-line build format `mgit --version` does, "+
			"differing only in the binary name it identifies itself by")
	assert.Contains(t, got, "commit: ")
	assert.Contains(t, got, "built: ")
}

// TestRun_Version_StartsNoDaemon proves --version answers on a host where the
// daemon could not run at all: no --socket is supplied, which every other path
// rejects with exit 2. Version has to work precisely when the daemon does not,
// because that is when someone is asking which build they have. It must also
// stay off stderr, so the answer is not tangled with the structured log.
// Refs: MGIT-83
func TestRun_Version_StartsNoDaemon(t *testing.T) {
	var out, logs bytes.Buffer

	code := run([]string{"--version"}, &out, &logs)

	require.Equal(t, 0, code)
	assert.Empty(t, logs.String(),
		"--version must not log, bind a socket, or probe a backend")
	assert.NotEmpty(t, out.String(), "the version goes to stdout, where a caller can capture it")
}

// TestRun_MissingSocket_StillFailsClosed is the control for the test above: the
// short-circuit must not have made the daemon lenient about its real flags.
func TestRun_MissingSocket_StillFailsClosed(t *testing.T) {
	var out, logs bytes.Buffer

	code := run(nil, &out, &logs)

	assert.Equal(t, 2, code, "a daemon started without --socket must still refuse")
	assert.Contains(t, logs.String(), "socket")
}
