package main

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/backend/libkrun"
)

// TestMain lets this test binary stand in for mgit-sandboxd when --vmm
// re-execs it as the libkrun probe child: the child's argv is routed through
// the real run(), so the dispatch under test is the shipped one. Without it
// the re-exec'd binary would run this whole suite again. Refs: MGIT-229
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == libkrun.ProbeCommand {
		os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

// --vmm answers "which VMM does this build link, where did its libraries
// resolve, and can it boot a guest" as JSON on stdout, and — like --version —
// starts nothing: no socket, no host root. It is the question doctor asks,
// because a daemon that loads is not yet a daemon that can boot (libkrun loads
// libkrunfw lazily). Refs: MGIT-229, MGIT-206
func TestRun_VMMFlag_ReportsTheLinkedVMMAndStartsNoDaemon(t *testing.T) {
	var out, logs bytes.Buffer
	code := run([]string{"--vmm"}, &out, &logs)
	require.Equal(t, 0, code, "stderr: %s", logs.String())

	var r model.VMMReport
	require.NoError(t, json.Unmarshal(out.Bytes(), &r), "stdout must be one JSON report: %q", out.String())
	assert.Equal(t, wantLinkedVMM, r.VMM)
	assert.Empty(t, logs.String(), "--vmm must not start (or log) a daemon")
}

// The flag is mutually exclusive with running a daemon: a --socket beside it
// is not served. Refs: MGIT-229
func TestRun_VMMFlag_WithASocket_StillServesNothing(t *testing.T) {
	sock := t.TempDir() + "/d.sock"
	var out, logs bytes.Buffer
	require.Equal(t, 0, run([]string{"--vmm", "--socket", sock}, &out, &logs))
	assert.NoFileExists(t, sock)
}
