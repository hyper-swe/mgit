package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/sandboxd/daemonrec"
)

// THE MACHINE'S DAEMONS DO NOT DECIDE A DOCTOR WIRING TEST (MGIT-262).
// Two doctor rows judge every sandbox daemon on the host: a live daemon whose
// repository is gone is leaked, and two live daemons on one root are
// duplicates. Either fails its row, and a failed row sets exit 1. The wiring
// tests ran the whole doctor with the host's real daemon records, so on a
// machine where other sessions start and remove daemons, "every check ok"
// failed for a reason that had nothing to do with the code under test.
//
// A leaked, live daemon is planted under an isolated runtime directory (never
// the machine's real records): the test process's own pid, so it is alive,
// and a repository root that does not exist, so it is leaked. The wiring
// test's every-check-ok case must still exit zero.
func TestDoctor_ExitCode_TheHostsDaemonsDoNotDecideIt(t *testing.T) {
	plantLeakedHostDaemon(t)
	doctorRepo(t)
	cl := &fakeSandboxClient{execStdout: "127.0.0.1\tlocalhost\nsha256sum\ndrop_caches\n"}

	out, err := runDoctor(t, connecting(cl))

	require.NoError(t, err, "another session's leaked daemon must not fail this repository's doctor wiring test; output:\n%s", out)
}

// plantLeakedHostDaemon points the daemon records at a fresh runtime
// directory and writes one live, leaked daemon record into it.
func plantLeakedHostDaemon(t *testing.T) {
	t.Helper()
	base := isolatedDaemonRecords(t)
	plantHostDaemon(t, base, "leaked", filepath.Join(t.TempDir(), "removed-repository"))
}

// isolatedDaemonRecords points the daemon records at a fresh runtime
// directory, never the machine's, and returns the records' base directory.
func isolatedDaemonRecords(t *testing.T) string {
	t.Helper()
	runtimeDir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	return filepath.Join(runtimeDir, fmt.Sprintf("mgit-%d", os.Getuid()))
}

// plantHostDaemon writes one live daemon record (the test's own pid) serving
// root, under its own directory name.
func plantHostDaemon(t *testing.T, base, name, root string) {
	t.Helper()
	dir := filepath.Join(base, name)
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, daemonrec.Write(daemonrec.Record{
		PID:       os.Getpid(),
		RepoRoot:  root,
		Socket:    filepath.Join(dir, "d.sock"),
		StartedAt: time.Now(),
	}))
}

// The duplicates row is the other half: two live daemons serving one
// repository that still exists fail daemons/one-per-repository, and only
// that row. They must not decide a wiring test either. Refs: MGIT-262, MGIT-197
func TestDoctor_ExitCode_DuplicateHostDaemonsDoNotDecideIt(t *testing.T) {
	base := isolatedDaemonRecords(t)
	shared := t.TempDir()
	plantHostDaemon(t, base, "first", shared)
	plantHostDaemon(t, base, "second", shared)
	doctorRepo(t)
	cl := &fakeSandboxClient{execStdout: "127.0.0.1\tlocalhost\nsha256sum\ndrop_caches\n"}

	out, err := runDoctor(t, connecting(cl))

	require.NoError(t, err, "another session's duplicate daemons must not fail this repository's doctor wiring test; output:\n%s", out)
}

// noHostDaemons is the empty daemon listing the doctor wiring tests inject in
// place of the host's real records.
func noHostDaemons(context.Context) ([]daemonrec.Listed, error) { return nil, nil }

// THE PRODUCTION DOCTOR STILL READS THE HOST. The seam moved the lister into
// a dependency; doctorCmd, which main wires, must keep passing the real one,
// or the host-daemons rows would silently judge nothing for every user. The
// same planted leaked daemon fails its row here, through doctorCmd.
// Refs: MGIT-262, MGIT-162
func TestDoctorCmd_ProductionWiringReadsTheHostsDaemons(t *testing.T) {
	plantLeakedHostDaemon(t)
	doctorRepo(t)
	cl := &fakeSandboxClient{execStdout: "127.0.0.1\tlocalhost\nsha256sum\ndrop_caches\n"}
	root := rootCmd()
	for _, c := range root.Commands() {
		if c.Name() == "doctor" {
			root.RemoveCommand(c)
			break
		}
	}
	root.AddCommand(hostOnly(doctorCmd(connecting(cl))))
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"doctor"})

	err := root.ExecuteContext(context.Background())

	var ee *exitError
	require.ErrorAs(t, err, &ee, "the production doctor must fail on a leaked host daemon; output:\n%s", out.String())
	assert.Equal(t, 1, ee.code)
	assert.Regexp(t, `FAIL\s+daemons/host`, out.String())
}
