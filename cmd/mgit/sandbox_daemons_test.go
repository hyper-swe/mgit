package main

import (
	"bytes"
	"context"
	"errors"
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
	root := &cobra.Command{Use: "sandbox"}
	root.AddCommand(sandboxDaemonsCmd(deps))
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}
