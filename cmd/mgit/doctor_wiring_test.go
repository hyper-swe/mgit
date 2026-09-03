package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/doctor"
)

// runDoctor executes `mgit doctor` through the REAL root command, with only
// the daemon connector swapped.
//
// Driving doctorCmd standalone would skip the root's PersistentPreRun — which
// is where MGIT-157's usage-dump fix lives — and a test that bypasses the fix
// cannot see it regress. Building the root and replacing one subcommand keeps
// every other piece of the production wiring in place.
func runDoctor(t *testing.T, connect connectFunc, args ...string) (string, error) {
	t.Helper()
	root := rootCmd()
	for _, c := range root.Commands() {
		if c.Name() == "doctor" {
			root.RemoveCommand(c)
			break
		}
	}
	root.AddCommand(hostOnly(doctorCmd(connect)))

	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(append([]string{"doctor"}, args...))
	err := root.ExecuteContext(context.Background())
	return out.String(), err
}

// connecting returns a connectFunc handing back the given client.
func connecting(c sandboxClient) connectFunc {
	return func(context.Context) (sandboxClient, error) { return c, nil }
}

// unreachable returns a connectFunc that cannot reach a daemon.
func unreachable(err error) connectFunc {
	return func(context.Context) (sandboxClient, error) { return nil, err }
}

// TestDoctor_ExitCode is doctor's contract with every caller that is not a
// human: a script, a CI gate, or an agent reading $?.
//
// The rule under test is the one the framework was built around — a check that
// could NOT run must not fail the exit code, because a gate that fails for
// reasons the reader cannot act on stops being consulted at all. Nothing
// asserted it end-to-end; the CLI's own exit branch had no coverage.
// Refs: MGIT-162
func TestDoctor_ExitCode_OnlyAFailedCheckIsNonZero(t *testing.T) {
	tests := []struct {
		name     string
		hostsOut string // what the guest's probe answers
		execCode int
		execErr  error
		wantCode int
	}{
		{
			name:     "every_check_ok_exits_zero",
			hostsOut: "127.0.0.1\tlocalhost\n",
			wantCode: 0,
		},
		{
			name:     "a_failing_check_exits_one",
			hostsOut: "", // the MGIT-159 condition: nothing resolves
			wantCode: 1,
		},
		{
			name:     "a_check_that_could_not_run_does_NOT_fail_the_gate",
			execErr:  errors.New("daemon went away"),
			wantCode: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doctorRepo(t)
			cl := &fakeSandboxClient{
				execStdout: tt.hostsOut, execCode: tt.execCode, execErr: tt.execErr,
			}
			out, err := runDoctor(t, connecting(cl))

			if tt.wantCode == 0 {
				require.NoError(t, err, "output was:\n%s", out)
				return
			}
			var ee *exitError
			require.ErrorAs(t, err, &ee, "a failing check must set a non-zero exit; output:\n%s", out)
			assert.Equal(t, tt.wantCode, ee.code)
		})
	}
}

// The report is the message; the exit code is only the verdict. A failing
// doctor must not also print cobra's "Error: exit status 1", which names
// nothing and reads as a malfunction OF doctor rather than a finding BY it.
// Refs: MGIT-162
func TestDoctor_AFailingCheck_PrintsTheFindingAndNoCobraErrorLine(t *testing.T) {
	doctorRepo(t)

	out, err := runDoctor(t, connecting(&fakeSandboxClient{execStdout: ""}))

	require.Error(t, err)
	assert.Contains(t, out, "FAIL")
	assert.Contains(t, out, "guest/localhost")
	assert.Contains(t, out, "MGIT-159", "the finding names the incident it converts")
	assert.Contains(t, out, "remedy:", "a failure without a remedy has moved the mystery")
	assert.NotContains(t, out, "Error: exit status",
		"the exit code is the verdict; a second error line reads as a malfunction")
	assert.NotContains(t, out, "Usage:",
		"a runtime finding is not a usage error")
}

// --json must carry the SAME verdicts as the text report — a harness reading
// one and a human reading the other must never be told different things.
// Asserted by comparing the two renderings of one run, not by pinning a
// literal document. Refs: MGIT-162
func TestDoctor_JSON_CarriesTheSameVerdictsAsTheText(t *testing.T) {
	doctorRepo(t)
	cl := &fakeSandboxClient{execStdout: ""}

	textOut, textErr := runDoctor(t, connecting(cl))
	jsonOut, jsonErr := runDoctor(t, connecting(cl), "--json")

	require.Error(t, textErr)
	require.Error(t, jsonErr, "the exit code must not depend on the output format")

	var report struct {
		Checks []doctor.Result `json:"checks"`
	}
	require.NoError(t, json.Unmarshal([]byte(jsonOut), &report), "raw: %s", jsonOut)
	require.NotEmpty(t, report.Checks)

	for _, c := range report.Checks {
		assert.NotEmpty(t, c.Name)
		assert.NotEmpty(t, c.Incident, "%s must name its incident in JSON too", c.Name)
		assert.Contains(t, textOut, c.Name,
			"a check present in JSON must be present in the text report")
		if c.Status == doctor.StatusFailed {
			assert.NotEmpty(t, c.Remedy, "%s reports a failure with no remedy", c.Name)
		}
		if c.Status == doctor.StatusNotChecked {
			assert.NotEmpty(t, c.Reason, "%s could not run and does not say why", c.Name)
		}
	}
	assert.NotContains(t, jsonOut, "No check found",
		"the human reassurance line must not leak into the machine format")
}

// doctorChecks is the wiring between the framework and the real probes, and it
// had no coverage. The property is that the CLI registers every check the
// package defines — a check that exists but is never registered is a check
// nobody runs.
//
// The expected list is read from the doctor package's SOURCE, not from
// doctorChecks' own return value: comparing the wiring to itself would prove
// nothing, and the failure mode being guarded is precisely "a new check was
// written and never wired". Refs: MGIT-162, R-H300
func TestDoctorChecks_RegistersEveryCheckTheDoctorPackageDefines(t *testing.T) {
	defined := checkNamesFromSource(t)
	require.NotEmpty(t, defined, "the source scan found no checks; the pattern has drifted")

	dir := doctorRepo(t)
	app, err := openAppAt(dir)
	require.NoError(t, err)
	defer app.Close()

	wired := make(map[string]bool)
	for _, c := range doctorChecks(app, connecting(&fakeSandboxClient{})) {
		wired[c.Name()] = true
	}

	for _, name := range defined {
		assert.True(t, wired[name],
			"%s is defined in internal/doctor but never registered by the CLI, "+
				"so it never runs for anyone", name)
	}
}

// checkNamesFromSource reads the Name() strings out of internal/doctor's
// source, so the expectation comes from somewhere the CLI does not control.
func checkNamesFromSource(t *testing.T) []string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok, "cannot locate this test's own source")
	path := filepath.Join(filepath.Dir(thisFile), "..", "..", "internal", "doctor", "checks.go")
	b, err := os.ReadFile(filepath.Clean(path))
	require.NoError(t, err, "the scan must read the doctor package's source")
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.Contains(line, ") Name() string { return ") {
			continue
		}
		if i, j := strings.Index(line, `"`), strings.LastIndex(line, `"`); i >= 0 && j > i {
			out = append(out, line[i+1:j])
		}
	}
	return out
}

// probeGuestLocalhost is the CLI's real probe and had no coverage at all. Its
// job is to distinguish three outcomes the check renders very differently:
// an answer, no answer, and could-not-ask.
//
// The last is the one that matters. Reporting could-not-ask as a failure is
// the same defect as reporting it as a pass, one sign flipped: both hand the
// reader a verdict no evidence supports. Refs: MGIT-162, MGIT-159, MGIT-169
func TestProbeGuestLocalhost(t *testing.T) {
	tests := []struct {
		name      string
		taskID    string
		connect   connectFunc
		client    *fakeSandboxClient
		wantErr   string
		wantOut   string
		skipIssue string
	}{
		{
			name:    "no_bound_task_is_a_reason_not_a_verdict",
			taskID:  "",
			connect: connecting(&fakeSandboxClient{}),
			wantErr: "no sandbox",
		},
		{
			name:    "an_unreachable_daemon_is_a_reason_not_a_verdict",
			taskID:  "T-1",
			connect: unreachable(errors.New("connection refused")),
			wantErr: "no sandbox daemon reachable",
		},
		{
			name:    "an_exec_that_failed_is_a_reason_not_a_verdict",
			taskID:  "T-1",
			client:  &fakeSandboxClient{execErr: errors.New("guest is not running")},
			wantErr: "guest is not running",
		},
		{
			name:    "a_resolved_name_is_returned_verbatim",
			taskID:  "T-1",
			client:  &fakeSandboxClient{execStdout: "127.0.0.1\tlocalhost\n"},
			wantOut: "localhost",
		},
		{
			name:   "a_table_without_localhost_is_the_MGIT_159_condition_not_an_error",
			taskID: "T-1",
			// The probe reads /etc/hosts (MGIT-169). A table that maps
			// nothing to localhost IS the condition: an empty answer.
			client:  &fakeSandboxClient{execStdout: "10.0.0.1\tgateway\n"},
			wantOut: "",
		},
		{
			name:   "no_name_table_at_all_is_the_condition_too",
			taskID: "T-1",
			// cat exits 1 with "No such file": no table maps localhost.
			client:  &fakeSandboxClient{execCode: 1, execStderr: "cat: /etc/hosts: No such file or directory\n"},
			wantOut: "",
		},
		{
			name:   "a_probe_command_the_guest_does_not_have_is_a_reason_not_a_verdict",
			taskID: "T-1",
			// 127 is "command not found". The guest's name table may be
			// perfectly correct; nothing here establishes otherwise.
			client:  &fakeSandboxClient{execCode: 127, execStderr: "sh: cat: not found"},
			wantErr: "127",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.skipIssue != "" {
				t.Skip(tt.skipIssue)
			}
			connect := tt.connect
			if connect == nil {
				connect = connecting(tt.client)
			}
			got, err := probeGuestLocalhost(context.Background(), connect, tt.taskID)

			if tt.wantErr != "" {
				require.Error(t, err, "an unanswerable probe must report WHY, not an empty answer")
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			if tt.wantOut == "" {
				assert.Empty(t, got)
				return
			}
			assert.Contains(t, got, tt.wantOut)
		})
	}
}

// The probe must ask the guest bound to THIS worktree, not some other task. A
// probe that asked the wrong sandbox would report a healthy guest while the
// operator's own is broken. Refs: MGIT-162
func TestProbeGuestLocalhost_AsksTheBoundTask(t *testing.T) {
	cl := &fakeSandboxClient{execStdout: "127.0.0.1 localhost"}

	_, err := probeGuestLocalhost(context.Background(), connecting(cl), "MGIT-42")

	require.NoError(t, err)
	assert.Equal(t, "MGIT-42", cl.execTask, "the probe must target the bound task")
	assert.NotEmpty(t, cl.execReq.Command, "and it must actually ask the guest something")
}

// The end-to-end shape PR #65 published: a repo with no sandbox reports the
// tree check and a not-checked guest check, exits zero, and says plainly that
// the `?` is not a pass. Refs: MGIT-162
func TestDoctor_NoSandbox_ReportsNotCheckedAndSaysItIsNotAPass(t *testing.T) {
	doctorRepo(t)

	out, err := runDoctor(t, unreachable(errors.New("no daemon")))

	require.NoError(t, err, "a check that could not run must not fail the gate")
	assert.Contains(t, out, "guest/localhost")
	assert.Contains(t, out, "why not:", "an un-runnable check must say why")
	assert.Contains(t, out, "absence of evidence, not a pass")
	assert.NotContains(t, out, "ok    guest/localhost",
		"a check that never ran must not wear the ok marker")
}

// doctorRepo makes an mgit repo, adds a task-bound worktree, and chdirs into
// it — so the guest check has a task to ask about rather than short-circuiting
// on "this directory is not bound to a task worktree".
//
// It deliberately does NOT plant a nested .git: since MGIT-157 was fixed, a
// nested store is excluded per-component and never reaches the recorded tree,
// so a worktree cannot be poisoned through the CLI any more. Proving
// tree/nested-git fires needs a tree written by a PRE-FIX mgit, which is built
// with plumbing and tested in the store package where that is natural.
func doctorRepo(t *testing.T) string {
	t.Helper()
	_ = projectWithGit(t)
	wt := filepath.Join(t.TempDir(), "wt")
	require.NoError(t, runCLI(t, "worktree", "add", wt, "--task-id", "DOC-1"))
	t.Chdir(wt)
	return wt
}

// MGIT-157's door defect, pinned on doctor specifically: a runtime FINDING
// must not drag twenty lines of flags behind it, which pushes the one line
// that matters off the top of a terminal and implies the user mistyped
// something.
//
// Its other half is pinned with it, because a fix that silenced usage
// declaratively would suppress it for GENUINE usage errors too — where the
// dump is exactly what a reader wants. One test, both halves, so neither can
// be traded for the other. Refs: MGIT-157, MGIT-162
func TestDoctor_UsageDump_OnlyForARealUsageError(t *testing.T) {
	doctorRepo(t)

	finding, findingErr := runDoctor(t, connecting(&fakeSandboxClient{execStdout: ""}))
	require.Error(t, findingErr, "the premise: this run found something")
	assert.NotContains(t, finding, "Usage:",
		"a runtime finding is not a usage error")
	assert.NotContains(t, finding, "--json",
		"nor should the flag list appear by any other route")

	mistyped, mistypedErr := runDoctor(t, connecting(&fakeSandboxClient{}), "--no-such-flag")
	require.Error(t, mistypedErr)
	assert.Contains(t, mistyped, "Usage:",
		"a genuine usage error still gets the dump, which is what the reader needs")
}
