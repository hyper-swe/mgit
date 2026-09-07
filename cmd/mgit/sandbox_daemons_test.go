package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/sandboxd/daemonrec"
)

var listed = []daemonrec.Listed{
	{Record: daemonrec.Record{PID: 7803, RepoRoot: "/Users/x/work/repo", Version: "0.6.1"}, Status: daemonrec.Status{Alive: true, Age: 14 * 24 * time.Hour}},
	{Record: daemonrec.Record{PID: 6445, RepoRoot: "/var/folders/T/tmp.s0nX", Version: "dev"}, Status: daemonrec.Status{Alive: true, TempRoot: true, RootGone: true, Age: 13 * 24 * time.Hour}},
	{Record: daemonrec.Record{PID: 9999, RepoRoot: "/Users/x/work/old"}, Status: daemonrec.Status{Alive: false, Age: time.Hour}},
}

// `mgit sandbox daemons` is the host-wide view MGIT-185 asked for: every
// daemon with its pid, age, root and what is wrong with it, without ps.
// Refs: MGIT-191
func TestSandboxDaemons_ListsEveryDaemonWithItsRootAgeAndFlags(t *testing.T) {
	out, err := runDaemonsCmd(t, daemonsDeps{list: func(context.Context) ([]daemonrec.Listed, error) { return listed, nil }}, "daemons")
	require.NoError(t, err)
	assert.Contains(t, out, "7803")
	assert.Contains(t, out, "/Users/x/work/repo")
	assert.Contains(t, out, "14d")
	assert.Contains(t, out, "6445")
	assert.Contains(t, out, "temp-root")
	assert.Contains(t, out, "root-gone")
	assert.Contains(t, out, "9999")
	assert.Contains(t, out, "dead")
	assert.Less(t, strings.Index(out, "PID"), strings.Index(out, "7803"), "a header names the columns")
}

func TestSandboxDaemons_NoDaemons_SaysSo(t *testing.T) {
	out, err := runDaemonsCmd(t, daemonsDeps{list: func(context.Context) ([]daemonrec.Listed, error) { return nil, nil }}, "daemons")
	require.NoError(t, err)
	assert.Contains(t, out, "no sandbox daemons")
}

func TestSandboxDaemons_UnreadableBase_IsAnError(t *testing.T) {
	_, err := runDaemonsCmd(t, daemonsDeps{list: func(context.Context) ([]daemonrec.Listed, error) { return nil, errors.New("read runtime base: boom") }}, "daemons")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "boom")
}

// `stop --repo-root` signals exactly the daemon serving that root, by pid,
// and never anything else — five real repositories share this host.
// Refs: MGIT-191, MGIT-185
func TestSandboxDaemonsStop_SignalsOnlyTheDaemonOfThatRoot(t *testing.T) {
	var signaled []int
	deps := daemonsDeps{
		list:  func(context.Context) ([]daemonrec.Listed, error) { return listed, nil },
		kill:  func(pid int, sig syscall.Signal) error { signaled = append(signaled, pid); return nil },
		alive: func(pid int) bool { return false },
	}
	out, err := runDaemonsCmd(t, deps, "daemons", "stop", "--repo-root", "/var/folders/T/tmp.s0nX")
	require.NoError(t, err)
	assert.Equal(t, []int{6445}, signaled)
	assert.Contains(t, out, "6445")
	assert.Contains(t, out, "stopped")
}

func TestSandboxDaemonsStop_UnknownRoot_RefusesNamingIt(t *testing.T) {
	var signaled []int
	deps := daemonsDeps{
		list: func(context.Context) ([]daemonrec.Listed, error) { return listed, nil },
		kill: func(pid int, sig syscall.Signal) error { signaled = append(signaled, pid); return nil },
	}
	_, err := runDaemonsCmd(t, deps, "daemons", "stop", "--repo-root", "/nowhere")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "/nowhere")
	assert.Empty(t, signaled, "an unknown root signals nothing")
}

// runDaemonsCmd executes the daemons verbs against injected host facts.
func runDaemonsCmd(t *testing.T, deps daemonsDeps, args ...string) (string, error) {
	t.Helper()
	if deps.clock == nil {
		deps.clock = time.Now
	}
	if deps.alive == nil {
		deps.alive = func(int) bool { return false }
	}
	if deps.argv == nil {
		// A command line that names the daemon; records in these tests carry
		// no socket unless the identity check is the subject.
		deps.argv = func(int) (string, error) { return "mgit-sandboxd", nil }
	}
	root := &cobra.Command{Use: "sandbox"}
	root.AddCommand(sandboxDaemonsCmd(deps))
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

// A record names a pid; pids are reused. Before signaling, stop checks that
// the process at that pid is the daemon the record describes — its command
// line names mgit-sandboxd and the record's socket — and otherwise signals
// nothing, removes the stale record, and says so. Refs: MGIT-191
func TestSandboxDaemonsStop_RefusesAPidThatIsNotTheRecordedDaemon(t *testing.T) {
	dir := t.TempDir()
	rec := daemonrec.Record{PID: 4242, RepoRoot: "/work/repo", Socket: dir + "/d.sock"}
	require.NoError(t, daemonrec.Write(rec))
	var signaled []int
	deps := daemonsDeps{
		list: func(context.Context) ([]daemonrec.Listed, error) {
			return []daemonrec.Listed{{Record: rec, Status: daemonrec.Status{Alive: true}}}, nil
		},
		kill:  func(pid int, sig syscall.Signal) error { signaled = append(signaled, pid); return nil },
		alive: func(int) bool { return true },
		argv:  func(int) (string, error) { return "/usr/bin/some-other-program --serve", nil },
	}
	_, err := runDaemonsCmd(t, deps, "daemons", "stop", "--repo-root", "/work/repo")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "4242")
	assert.Contains(t, err.Error(), "some-other-program")
	assert.Empty(t, signaled, "an unrelated process at a reused pid must never be signaled")
	_, statErr := os.Stat(dir + "/" + daemonrec.FileName)
	assert.True(t, os.IsNotExist(statErr), "the stale record is removed")
}

func TestSandboxDaemonsStop_SignalsWhenTheProcessIsTheRecordedDaemon(t *testing.T) {
	rec := daemonrec.Record{PID: 4242, RepoRoot: "/work/repo", Socket: "/run/x/d.sock"}
	var signaled []int
	deps := daemonsDeps{
		list: func(context.Context) ([]daemonrec.Listed, error) {
			return []daemonrec.Listed{{Record: rec, Status: daemonrec.Status{Alive: true}}}, nil
		},
		kill:  func(pid int, sig syscall.Signal) error { signaled = append(signaled, pid); return nil },
		alive: func(int) bool { return false },
		argv: func(int) (string, error) {
			return "/usr/local/bin/mgit-sandboxd --socket /run/x/d.sock --host-root /work/repo/.mgit/sandbox", nil
		},
	}
	_, err := runDaemonsCmd(t, deps, "daemons", "stop", "--repo-root", "/work/repo")
	require.NoError(t, err)
	assert.Equal(t, []int{4242}, signaled)
}

// The root is matched as a path, not as bytes: a trailing slash or a
// symlinked prefix names the same repository. Refs: MGIT-191, MGIT-166
func TestSandboxDaemonsStop_MatchesTheRootAsAPath(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	require.NoError(t, os.Symlink(real, link))
	rec := daemonrec.Record{PID: 4242, RepoRoot: real, Socket: "/run/x/d.sock"}
	var signaled []int
	deps := daemonsDeps{
		list: func(context.Context) ([]daemonrec.Listed, error) {
			return []daemonrec.Listed{{Record: rec, Status: daemonrec.Status{Alive: true}}}, nil
		},
		kill:  func(pid int, sig syscall.Signal) error { signaled = append(signaled, pid); return nil },
		alive: func(int) bool { return false },
		argv:  func(int) (string, error) { return "mgit-sandboxd --socket /run/x/d.sock", nil },
	}
	for _, spelled := range []string{real + "/", link} {
		signaled = nil
		_, err := runDaemonsCmd(t, deps, "daemons", "stop", "--repo-root", spelled)
		require.NoError(t, err, spelled)
		assert.Equal(t, []int{4242}, signaled, spelled)
	}
}

// The word an operator scans for is pinned. Refs: MGIT-191
func TestSandboxDaemons_ALeakedDaemonIsMarkedLEAKED(t *testing.T) {
	out, err := runDaemonsCmd(t, daemonsDeps{list: func(context.Context) ([]daemonrec.Listed, error) { return listed, nil }}, "daemons")
	require.NoError(t, err)
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "6445") {
			assert.Contains(t, line, "LEAKED")
		}
		if strings.Contains(line, "7803") {
			assert.NotContains(t, line, "LEAKED", "a healthy repository daemon is not a leak")
		}
	}
}

// The identity check has two halves and both are pinned: the daemon's name,
// and the record's socket — a mgit-sandboxd serving some OTHER socket is not
// this daemon either. And an unreadable command line is a refusal that keeps
// the record: not knowing is not evidence that the record is stale.
// Refs: MGIT-191, R-H300
func TestSandboxDaemonsStop_IdentityCheck_BothHalvesAndTheUnreadableCase(t *testing.T) {
	tests := []struct {
		name       string
		argv       string
		argvErr    error
		wantSignal bool
		wantRecord bool // the record still exists afterwards
	}{
		{"the_recorded_daemon", "/usr/local/bin/mgit-sandboxd --socket SOCK --host-root /w/.mgit/sandbox", nil, true, true},
		{"a_daemon_serving_another_socket", "/usr/local/bin/mgit-sandboxd --socket /run/other/d.sock", nil, false, false},
		{"not_a_daemon_at_all", "/usr/bin/vim SOCK", nil, false, false},
		{"command_line_unreadable", "", errors.New("ps: no such process table"), false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			rec := daemonrec.Record{PID: 4242, RepoRoot: "/w", Socket: dir + "/d.sock"}
			require.NoError(t, daemonrec.Write(rec))
			var signaled []int
			deps := daemonsDeps{
				list: func(context.Context) ([]daemonrec.Listed, error) {
					return []daemonrec.Listed{{Record: rec, Status: daemonrec.Status{Alive: true}}}, nil
				},
				kill:  func(pid int, sig syscall.Signal) error { signaled = append(signaled, pid); return nil },
				alive: func(int) bool { return false },
				argv: func(int) (string, error) {
					return strings.ReplaceAll(tt.argv, "SOCK", rec.Socket), tt.argvErr
				},
			}
			_, err := runDaemonsCmd(t, deps, "daemons", "stop", "--repo-root", "/w")
			if tt.wantSignal {
				require.NoError(t, err)
				assert.Equal(t, []int{4242}, signaled)
			} else {
				require.Error(t, err)
				assert.Empty(t, signaled)
			}
			_, statErr := os.Stat(dir + "/" + daemonrec.FileName)
			assert.Equal(t, tt.wantRecord, statErr == nil, "record kept?")
		})
	}
}
