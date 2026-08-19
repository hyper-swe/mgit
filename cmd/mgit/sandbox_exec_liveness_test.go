package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// errStalledDaemon is the failure the client raises when mgit-sandboxd stops
// beating on an open exec stream — wrapped exactly as the client wraps it, so
// these tests exercise the real sentinel rather than a stand-in.
var errStalledDaemon = fmt.Errorf("%w: no liveness beat for 15s while the command was still running",
	model.ErrSandboxDaemonUnresponsive)

// TestClassifyGuestFailure_DaemonStall_IsNotAGuestPhase pins the classifier's
// verdict on the one failure that carries no evidence about the guest.
//
// The classifier's DEFAULT is phaseLostServing — "the guest was reached and
// then stopped answering" — and that default is right for everything it was
// built to see. A daemon-side stall falling into it would be reported as a
// guest lost mid-command with a memory-cap advisory attached, which is
// MGIT-118's misdiagnosis rebuilt on a new cause. Refs: MGIT-133, MGIT-118
func TestClassifyGuestFailure_DaemonStall_IsNotAGuestPhase(t *testing.T) {
	got := classifyGuestFailure(errStalledDaemon, entitlementUnknown)
	assert.Equal(t, phaseDaemonStalled, got.phase,
		"a daemon that stopped answering was classified as a guest failure")
	assert.Empty(t, got.startDetail, "no VM-start evidence exists for a host-side stall")
}

// TestWriteGuestFailure_DaemonStall_NamesTheDaemonAndClearsTheGuest asserts the
// text, because being right and reading as right are different things here.
//
// The reader of this line is usually an agent, and MGIT-118 is what a wrong
// suspect costs: told its build had exhausted guest memory, it spent an hour
// shrinking a build that was never the problem. Refs: MGIT-133, MGIT-118
func TestWriteGuestFailure_DaemonStall_NamesTheDaemonAndClearsTheGuest(t *testing.T) {
	var out bytes.Buffer
	writeGuestFailure(&out, advisoryInfo(), guestFailure{phase: phaseDaemonStalled})
	got := out.String()

	assert.Contains(t, got, "sandbox daemon stopped answering")
	assert.Contains(t, got, "not of your command and not of the guest")
	assert.Contains(t, got, "may still be running inside the guest",
		"the command's fate is genuinely open and saying so is the honest report")
	assert.NotContains(t, got, "capped at",
		"the memory-cap advisory was printed for a failure of a host-side process (MGIT-118)")
	assert.NotContains(t, got, "stopped answering mid-command",
		"the guest-lost diagnosis was printed for a daemon-side stall")
	assert.NotContains(t, got, "never started",
		"the VM-start diagnosis was printed for a daemon that had already started one")
}

// TestSandboxExec_DaemonStall_DoesNotInterrogateTheStalledDaemon covers the
// second-order failure.
//
// The exec path consults the daemon for its diagnosis (which cap was in force,
// what state the sandbox is in). Doing that here means asking a daemon that has
// just demonstrated it cannot answer: the caller waits out the whole
// control-plane timeout on top of the stall, for facts the diagnosis does not
// use. Refs: MGIT-133
func TestSandboxExec_DaemonStall_DoesNotInterrogateTheStalledDaemon(t *testing.T) {
	fake := &fakeSandboxClient{execErr: errStalledDaemon, statusInfo: advisoryInfo()}
	out, err := runSandbox(okConnect(fake), "exec", "--task-id", "MGIT-133", "--", "go", "build", "./...")
	require.Error(t, err)

	assert.Zero(t, fake.statusCalls,
		"mgit asked the stalled daemon for sandbox details; that call hangs for the "+
			"control-plane timeout before yielding a diagnosis that never needed it")
	assert.Contains(t, out, "sandbox daemon stopped answering")
	assert.NotContains(t, out, "capped at")
}

// TestRunExec_DaemonStall_ReportsTheDaemon verifies `mgit run` — the verb an
// agent actually types — reaches the same diagnosis as `sandbox exec`.
// Refs: MGIT-133
func TestRunExec_DaemonStall_ReportsTheDaemon(t *testing.T) {
	dir := t.TempDir()
	fake := &fakeSandboxClient{execErr: errStalledDaemon, listResult: []model.SandboxInfo{{
		ID: "01JSB", TaskID: "MGIT-133", WorktreePath: canonicalPath(dir),
		State: model.StateRunning, CPUs: 2, MemoryMB: 3072,
	}}}
	cmd := newRunCmd(okConnect(fake), func() (string, error) { return dir, nil })
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--", "npm", "install"})
	require.Error(t, cmd.Execute())

	got := out.String()
	assert.Contains(t, got, "sandbox daemon stopped answering")
	assert.NotContains(t, got, "capped at", "a host-side stall must not implicate the guest memory cap")
	assert.NotContains(t, got, "stopped answering mid-command")
}

// TestExecTimeoutFlag_DefaultsToUnbounded_AndPopulatesTheRequest covers the
// second half of the ticket, and the reason it is a flag rather than a value.
//
// A timeout applied unasked is the MGIT-122 defect with a bigger number:
// whatever is chosen, some legitimate build exceeds it and dies for running
// long rather than for being stuck. So the default must be zero — the sandbox
// TTL governs — and a caller who genuinely wants a duration bound asks.
// Refs: MGIT-133, MGIT-122, FR-17.11
func TestExecTimeoutFlag_DefaultsToUnbounded_AndPopulatesTheRequest(t *testing.T) {
	t.Run("sandbox_exec_default_unbounded", func(t *testing.T) {
		fake := &fakeSandboxClient{}
		_, err := runSandbox(okConnect(fake), "exec", "--task-id", "MGIT-133", "--", "make")
		require.NoError(t, err)
		assert.Zero(t, fake.execReq.Timeout,
			"exec applied a timeout nobody asked for; that is MGIT-122 with a larger number")
	})
	t.Run("sandbox_exec_flag_populates_request", func(t *testing.T) {
		fake := &fakeSandboxClient{}
		_, err := runSandbox(okConnect(fake), "exec", "--task-id", "MGIT-133",
			"--timeout", "90s", "--", "make")
		require.NoError(t, err)
		assert.Equal(t, 90*time.Second, fake.execReq.Timeout)
	})
	t.Run("run_default_unbounded", func(t *testing.T) {
		fake, cmd := runCmdForTimeout(t, nil)
		require.NoError(t, cmd.Execute())
		assert.Zero(t, fake.execReq.Timeout)
	})
	t.Run("run_flag_populates_request", func(t *testing.T) {
		fake, cmd := runCmdForTimeout(t, []string{"--timeout", "2m"})
		require.NoError(t, cmd.Execute())
		assert.Equal(t, 2*time.Minute, fake.execReq.Timeout)
	})
}

// runCmdForTimeout builds an `mgit run` bound to a sandbox on a temp worktree,
// with flags prepended to a fixed command.
func runCmdForTimeout(t *testing.T, flags []string) (*fakeSandboxClient, *cobra.Command) {
	t.Helper()
	dir := t.TempDir()
	fake := &fakeSandboxClient{listResult: []model.SandboxInfo{{
		ID: "01JSB", TaskID: "MGIT-133", WorktreePath: canonicalPath(dir), State: model.StateRunning,
	}}}
	cmd := newRunCmd(okConnect(fake), func() (string, error) { return dir, nil })
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs(append(append([]string{}, flags...), "--", "make"))
	return fake, cmd
}

// TestExecTimeoutFlag_HelpExplainsWhyItIsUnbounded guards the one place a
// reader learns that the missing default is deliberate. A flag documented as
// "timeout (default 0)" invites the next maintainer to supply a "sensible"
// default and undo MGIT-122. Refs: MGIT-133, MGIT-122
func TestExecTimeoutFlag_HelpExplainsWhyItIsUnbounded(t *testing.T) {
	out, err := runSandbox(okConnect(&fakeSandboxClient{}), "exec", "--help")
	require.NoError(t, err)
	assert.Contains(t, out, "--timeout")
	assert.Contains(t, strings.ToLower(out), "default unbounded")
	assert.Contains(t, out, "liveness beats")
}
