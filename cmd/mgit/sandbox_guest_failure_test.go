package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// bootFailureText is the exec error mgit actually received on macOS/libkrun
// from a daemon lacking com.apple.security.hypervisor (MGIT-104, captured
// 2026-08-12): the launch fail-closed message from MGIT-92, with the guest's
// own console tail naming a VM-START failure. The guest never ran.
const bootFailureText = `sandbox exec: sandbox ensure-running: libkrun launch: ` +
	`guest never answered on its control channel within 15s: dial guest vsock: connect: no such file or directory
guest console (tail):
{"time":"2026-08-12T12:03:41Z","level":"ERROR","msg":"libkrun vm failed","event":"krun_vm_failed",` +
	`"error":"libkrun start vm: krun_start_enter: libkrun error -22"}`

// bootTimeoutText is a launch that failed closed with NO identifiable cause:
// the guest never answered and wrote nothing. mgit knows the phase but not the
// reason, so it must not invent one.
const bootTimeoutText = `sandbox exec: sandbox ensure-running: firecracker launch: ` +
	`guest never answered on its control channel within 15s: dial guest vsock: connection refused
guest console: empty (the guest never wrote anything)`

// advisoryInfo is a sandbox with a known ceiling, so any test that asserts the
// cap advisory is ABSENT proves the gate rather than a missing cap.
func advisoryInfo() *model.SandboxInfo {
	return &model.SandboxInfo{
		ID: "01JSB", TaskID: "MGIT-104", WorktreePath: "/work/a",
		State: model.StateRunning, CPUs: 2, MemoryMB: 3072,
	}
}

// TestClassifyGuestFailure_PhaseMatchesTheEvidence verifies mgit reads the
// phase off the evidence it already holds, rather than assuming one. A guest
// that never started and a guest lost while it was serving are different
// failures with different fixes. Refs: MGIT-104, R-H212
func TestClassifyGuestFailure_PhaseMatchesTheEvidence(t *testing.T) {
	tests := []struct {
		name          string
		err           error
		wantPhase     guestPhase
		wantDetail    string
		wantNoDetail  bool
		wantSameError bool
	}{
		{
			name:       "krun_start_failure_in_console_tail_is_never_started",
			err:        errors.New(bootFailureText),
			wantPhase:  phaseNeverStarted,
			wantDetail: "krun_start_enter: libkrun error -22",
		},
		{
			name:         "unexplained_boot_timeout_is_never_started_without_a_cause",
			err:          errors.New(bootTimeoutText),
			wantPhase:    phaseNeverStarted,
			wantNoDetail: true,
		},
		{
			name:         "in_process_sentinel_is_never_started",
			err:          fmt.Errorf("libkrun launch: %w", model.ErrGuestNotServing),
			wantPhase:    phaseNeverStarted,
			wantNoDetail: true,
		},
		{
			name:         "dropped_exec_channel_is_a_guest_lost_while_serving",
			err:          errors.New("sandbox exec: read frame: EOF"),
			wantPhase:    phaseLostServing,
			wantNoDetail: true,
		},
		{
			name:         "refused_vsock_dial_after_serving_is_a_guest_lost_while_serving",
			err:          errors.New("sandbox exec: guest vsock not ready: connect: connection refused"),
			wantPhase:    phaseLostServing,
			wantNoDetail: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyGuestFailure(tt.err, entitlementUnknown)
			assert.Equal(t, tt.wantPhase, got.phase)
			if tt.wantNoDetail {
				assert.Empty(t, got.startDetail, "no start failure may be claimed without evidence")
				return
			}
			assert.Contains(t, got.startDetail, tt.wantDetail)
		})
	}
}

// TestWriteGuestFailure_VMStartFailure_SurfacesItWithNoMemoryAdvisory is the
// defect this task exists for: mgit held a console tail with an explicit
// VM-start failure and appended the memory-cap advisory anyway, pointing the
// reader at --memory-mb when the guest had never run. Refs: MGIT-104
func TestWriteGuestFailure_VMStartFailure_SurfacesItWithNoMemoryAdvisory(t *testing.T) {
	var out bytes.Buffer
	writeGuestFailure(&out, advisoryInfo(), classifyGuestFailure(errors.New(bootFailureText), entitlementUnknown))
	got := out.String()

	assert.Contains(t, got, "never started", "the phase mgit observed is the phase it names")
	assert.Contains(t, got, "krun_start_enter: libkrun error -22", "the start failure is surfaced, not swallowed")
	assert.NotContains(t, got, "capped at", "a guest that never ran cannot have exhausted its memory")
	assert.NotContains(t, got, "--memory-mb", "the reader is never sent to raise a cap that was not the problem")
	assert.NotContains(t, got, "stopped answering mid-command",
		"a never-started guest is never described as stopping mid-command")
}

// TestWriteGuestFailure_MissingHypervisorEntitlement_NamesTheFix verifies the
// most common macOS first-hour failure is named with its actual remedy — an
// unsigned mgit-sandboxd cannot create a VM at all. Refs: MGIT-104, MGIT-64
func TestWriteGuestFailure_MissingHypervisorEntitlement_NamesTheFix(t *testing.T) {
	f := classifyGuestFailure(errors.New(bootFailureText), entitlementMissing)
	f.daemonPath = "/opt/homebrew/bin/mgit-sandboxd"
	var out bytes.Buffer
	writeGuestFailure(&out, advisoryInfo(), f)
	got := out.String()

	assert.Contains(t, got, "com.apple.security.hypervisor")
	assert.Contains(t, got, "codesign", "the fix is a command the reader can run")
	assert.Contains(t, got, "/opt/homebrew/bin/mgit-sandboxd",
		"the fix names the daemon the verdict is about, not whatever PATH resolves later")
	assert.NotContains(t, got, "capped at")
}

// TestWriteGuestFailure_MissingEntitlementWithoutAPath_FallsBackToTheName
// verifies the remedy stays runnable when no path was resolved. Refs: MGIT-104
func TestWriteGuestFailure_MissingEntitlementWithoutAPath_FallsBackToTheName(t *testing.T) {
	var out bytes.Buffer
	writeGuestFailure(&out, advisoryInfo(), classifyGuestFailure(errors.New(bootFailureText), entitlementMissing))
	assert.Contains(t, out.String(), "codesign --force --sign - --entitlements build/darwin/vz.entitlements mgit-sandboxd")
}

// TestWriteGuestFailure_UnexplainedBootFailure_ClaimsNoCause verifies mgit
// reports the phase and stops there when the console carries no reason: an
// invented cause is what MGIT-104 is about. Refs: MGIT-104
func TestWriteGuestFailure_UnexplainedBootFailure_ClaimsNoCause(t *testing.T) {
	var out bytes.Buffer
	writeGuestFailure(&out, advisoryInfo(), classifyGuestFailure(errors.New(bootTimeoutText), entitlementUnknown))
	got := out.String()

	assert.Contains(t, got, "never started")
	assert.NotContains(t, got, "capped at")
	assert.NotContains(t, got, "stopped answering mid-command")
	assert.NotContains(t, got, "com.apple.security.hypervisor",
		"no entitlement claim without a probe that found one missing")
}

// TestWriteGuestFailure_GuestLostWhileServing_KeepsTheCapAdvisory verifies the
// half of MGIT-95 that is correct still fires: a guest reached and then lost
// mid-command is exactly the shape real in-guest memory exhaustion takes, and
// the ceiling must still be named. Refs: MGIT-104, R-H212
func TestWriteGuestFailure_GuestLostWhileServing_KeepsTheCapAdvisory(t *testing.T) {
	var out bytes.Buffer
	writeGuestFailure(&out, advisoryInfo(), classifyGuestFailure(errGuestGone, entitlementUnknown))
	got := out.String()

	assert.Contains(t, got, "stopped answering mid-command")
	assert.Contains(t, got, "capped at 3072 MB of memory")
	assert.Contains(t, got, "do not reshape the build to fit the sandbox")
	assert.NotContains(t, got, "never started")
}

// TestEntitlementFromCodesign_ReadsTheDaemonSigning verifies the entitlement
// verdict is derived from what codesign actually reported — including the
// "cannot tell" case, which must never be reported as missing. Refs: MGIT-104
func TestEntitlementFromCodesign_ReadsTheDaemonSigning(t *testing.T) {
	tests := []struct {
		name   string
		output string
		err    error
		want   entitlementState
	}{
		{
			name:   "signed_with_the_hypervisor_key",
			output: "<key>com.apple.security.hypervisor</key><true/>",
			want:   entitlementPresent,
		},
		{
			name:   "signed_without_it",
			output: "<key>com.apple.security.virtualization</key><true/>",
			want:   entitlementMissing,
		},
		{
			name:   "not_signed_at_all",
			output: "/tmp/x/mgit-sandboxd: code object is not signed at all",
			err:    errors.New("exit status 1"),
			want:   entitlementMissing,
		},
		{
			name: "codesign_unavailable_is_unknown",
			err:  errors.New("exec: \"codesign\": executable file not found in $PATH"),
			want: entitlementUnknown,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, entitlementFromCodesign(tt.output, tt.err))
		})
	}
}

// TestRunExec_GuestNeverStarted_NoMemoryAdvisory drives the real `mgit run`
// cobra command over the captured failure: the wiring, not just the renderer.
// Refs: MGIT-104
func TestRunExec_GuestNeverStarted_NoMemoryAdvisory(t *testing.T) {
	dir := t.TempDir()
	fake := &fakeSandboxClient{execErr: errors.New(bootFailureText), listResult: []model.SandboxInfo{{
		ID: "01JSB", TaskID: "MGIT-104", WorktreePath: canonicalPath(dir),
		State: model.StateCreated, CPUs: 2, MemoryMB: 3072,
	}}}
	cmd := newRunCmd(okConnect(fake), func() (string, error) { return dir, nil })
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--", "npm", "run", "build"})
	require.Error(t, cmd.Execute())

	got := out.String()
	assert.Contains(t, got, "never started")
	assert.Contains(t, got, "krun_start_enter")
	assert.NotContains(t, got, "capped at")
	assert.NotContains(t, got, "stopped answering mid-command")
}

// TestSandboxExec_GuestNeverStarted_NoMemoryAdvisory verifies `mgit sandbox
// exec` — the second call site of the same advisory — is gated identically.
// Refs: MGIT-104
func TestSandboxExec_GuestNeverStarted_NoMemoryAdvisory(t *testing.T) {
	fake := &fakeSandboxClient{execErr: errors.New(bootFailureText), statusInfo: advisoryInfo()}
	out, err := runSandbox(okConnect(fake), "exec", "--task-id", "MGIT-104", "--", "npm", "run", "build")
	require.Error(t, err)

	assert.Contains(t, out, "never started")
	assert.NotContains(t, out, "capped at")
	assert.NotContains(t, out, "stopped answering mid-command")
}

// TestSandboxExec_GuestLostWhileServing_StillNamesTheCap verifies the MGIT-95
// advisory is untouched on the path it was built for. Refs: MGIT-104, R-H212
func TestSandboxExec_GuestLostWhileServing_StillNamesTheCap(t *testing.T) {
	fake := &fakeSandboxClient{execErr: errGuestGone, statusInfo: advisoryInfo()}
	out, err := runSandbox(okConnect(fake), "exec", "--task-id", "MGIT-104", "--", "npm", "run", "build")
	require.Error(t, err)

	assert.Contains(t, out, "stopped answering mid-command")
	assert.Contains(t, out, "capped at 3072 MB of memory")
}

// TestProbeHypervisorEntitlement_OffDarwin_IsUnknown verifies the probe never
// asserts a verdict on a platform where the entitlement does not exist — an
// invented "missing" would be the same defect in a new place. Refs: MGIT-104
func TestProbeHypervisorEntitlement_OffDarwin_IsUnknown(t *testing.T) {
	got, path := probeHypervisorEntitlement(context.Background())
	if runtime.GOOS != "darwin" {
		assert.Equal(t, entitlementUnknown, got)
		assert.Empty(t, path)
		return
	}
	// On darwin the verdict depends on how THIS machine installed the daemon;
	// all three states are legitimate, and none may panic or hang.
	assert.Contains(t, []entitlementState{entitlementUnknown, entitlementPresent, entitlementMissing}, got)
}

// TestWriteGuestFailure_UnknownSandbox_StillReportsThePhase verifies the
// diagnosis survives a failure discovered before the sandbox record is known:
// the phase is the part that must never be lost, and a nil sandbox must not
// panic on the path that is already failing. Refs: MGIT-104
func TestWriteGuestFailure_UnknownSandbox_StillReportsThePhase(t *testing.T) {
	var out bytes.Buffer
	writeGuestFailure(&out, nil, classifyGuestFailure(errors.New(bootTimeoutText), entitlementUnknown))
	got := out.String()

	assert.Contains(t, got, "never started")
	assert.Contains(t, got, "<task>", "a suggested command names a placeholder, never a wrong task")
	assert.NotContains(t, got, "capped at")
}

// TestVMStartFailure_NonJSONConsoleLine_QuotedAsWritten verifies a guest that
// logs plain text is surfaced too — the markers, not the encoding, are the
// evidence. Refs: MGIT-104
func TestVMStartFailure_NonJSONConsoleLine_QuotedAsWritten(t *testing.T) {
	got := vmStartFailure("launch failed\n  krun_start_enter returned -22 (EINVAL)\n")
	assert.Equal(t, "krun_start_enter returned -22 (EINVAL)", got)
}

// TestClassifyGuestFailure_NoError_ClaimsNoStartFailure guards the degenerate
// call: with nothing to read, mgit may not conclude the guest never started.
// Refs: MGIT-104
func TestClassifyGuestFailure_NoError_ClaimsNoStartFailure(t *testing.T) {
	got := classifyGuestFailure(nil, entitlementUnknown)
	assert.Equal(t, phaseLostServing, got.phase)
	assert.Empty(t, got.startDetail)
}

// TestVMStartFailure_Truncates verifies a pathological console line cannot turn
// the diagnosis into a wall of text. Refs: MGIT-104
func TestVMStartFailure_Truncates(t *testing.T) {
	line := `{"event":"krun_vm_failed","error":"` + strings.Repeat("x", 2000) + `"}`
	got := vmStartFailure("launch failed\n" + line)
	assert.NotEmpty(t, got)
	assert.LessOrEqual(t, len(got), startDetailMax+len(startDetailEllipsis))
}
