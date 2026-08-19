package main

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/controlproto"
	"github.com/hyper-swe/mgit/internal/model"
)

// errVersionSkew is a mismatch wrapped exactly as the sandbox client wraps
// one, so these tests exercise the real sentinel rather than a stand-in.
var errVersionSkew = fmt.Errorf("%w", &skewStubError{msg: controlproto.SkewMessage(
	controlproto.Peer{Protocol: 2, Version: "0.6.0 (commit: aaa)"},
	controlproto.Peer{Protocol: 1})})

// skewStubError mirrors the client's skew error: the full remedy as text, the
// sentinel as identity.
type skewStubError struct{ msg string }

func (e *skewStubError) Error() string { return e.msg }
func (e *skewStubError) Unwrap() error { return model.ErrSandboxVersionSkew }

// TestClassifyGuestFailure_VersionSkew_IsNotAGuestPhase pins the classifier on
// the failure that carries no evidence about the guest AT ALL: the two host
// binaries never agreed, so nothing was sent and nothing ran.
//
// The classifier's default is phaseLostServing, and a skew falling into it is
// reported as a guest lost mid-command with a memory-cap advisory attached —
// which is exactly what mgit 0.5.0 does today and exactly why this ticket is
// release-gating. Refs: MGIT-136, MGIT-118
func TestClassifyGuestFailure_VersionSkew_IsNotAGuestPhase(t *testing.T) {
	got := classifyGuestFailure(errVersionSkew, entitlementUnknown)
	assert.Equal(t, phaseVersionSkew, got.phase,
		"a version mismatch was classified as a guest failure")
	assert.Empty(t, got.startDetail, "no VM-start evidence exists for a mismatch")
}

// TestClassifyGuestFailure_VersionSkew_MatchedByTextToo verifies the verdict
// survives a crossing that carries no error identity.
//
// An exec failure can reach the CLI as a plain string — the daemon's result
// frame has no place to put a wrapped error — so identity alone would let a
// mismatch relayed that way fall straight through to the guest default. The
// daemon-stall check is matched by text for the same reason. Refs: MGIT-136, MGIT-133
func TestClassifyGuestFailure_VersionSkew_MatchedByTextToo(t *testing.T) {
	relayed := errors.New("sandbox exec: " + model.ErrSandboxVersionSkew.Error() +
		" — nothing was run in the guest; see the upgrade instructions above")
	require.NotErrorIs(t, relayed, model.ErrSandboxVersionSkew, "this failure carries no identity")
	assert.Equal(t, phaseVersionSkew, classifyGuestFailure(relayed, entitlementUnknown).phase)
}

// TestClassifyGuestFailure_VersionSkew_SettledBeforeEveryOtherSignal is the
// ordering guard.
//
// A skew message can legitimately arrive carrying other text — a wrapped
// transport error, a console tail already on the buffer — and the phase must
// still be the mismatch. Whichever signal wins here decides whether the reader
// is told to upgrade or told to resize a sandbox. Refs: MGIT-136, MGIT-104
func TestClassifyGuestFailure_VersionSkew_SettledBeforeEveryOtherSignal(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{"with_vm_start_marker", fmt.Errorf("%w\nkrun_vm_failed: no hypervisor entitlement", errVersionSkew)},
		{"with_guest_not_serving", fmt.Errorf("%w: %w", errVersionSkew, model.ErrGuestNotServing)},
		{"with_daemon_stall_text", fmt.Errorf("%w (%s)", errVersionSkew,
			model.ErrSandboxDaemonUnresponsive.Error())},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, phaseVersionSkew, classifyGuestFailure(tt.err, entitlementUnknown).phase,
				"another signal outranked the version mismatch")
		})
	}
}

// TestWriteGuestFailure_VersionSkew_NeverReachesTheMemoryAdvisory asserts the
// negative this ticket exists for, explicitly.
//
// Three times a host-side fault has been reported to an agent as in-guest
// memory exhaustion (MGIT-104, MGIT-108, MGIT-118) and MGIT-136 is the fourth
// route. The rendered diagnosis for a mismatch must not name the cap, must not
// suggest resizing, and must not claim anything happened inside a guest.
// Refs: MGIT-136, MGIT-118
func TestWriteGuestFailure_VersionSkew_NeverReachesTheMemoryAdvisory(t *testing.T) {
	var out bytes.Buffer
	writeGuestFailure(&out, advisoryInfo(), guestFailure{phase: phaseVersionSkew})
	got := out.String()

	for _, forbidden := range []string{
		"capped at", "memory-mb", "--memory-mb", "Memory exhaustion",
		"stopped answering mid-command", "never started",
	} {
		assert.NotContains(t, got, forbidden,
			"a version mismatch rendered a guest diagnosis: %q", forbidden)
	}
	// Not even the word: the advisory this phase exists to avoid is reached by
	// mentioning the cap at all, and "do not resize" reads to a hurried agent
	// as "resize". The sibling phases carry an explicit NEGATION instead, and
	// so does this one — assert that it is only ever the negation.
	assert.NotContains(t, strings.ToLower(got), "memory")
	assert.Contains(t, got, "do not resize anything and do not reshape the build")
}

// TestWriteGuestFailure_VersionSkew_SaysWhatItIsAndWhatToRun asserts the
// positive half: the reader must know the next move without a hunt.
// Refs: MGIT-136, MGIT-132
func TestWriteGuestFailure_VersionSkew_SaysWhatItIsAndWhatToRun(t *testing.T) {
	var out bytes.Buffer
	writeGuestFailure(&out, advisoryInfo(), guestFailure{phase: phaseVersionSkew})
	got := out.String()

	assert.Contains(t, got, model.ErrSandboxVersionSkew.Error())
	assert.Contains(t, got, "nothing ran",
		"the reader must know their command was never sent anywhere")
	assert.Contains(t, got, "mgit --version")
	assert.Contains(t, got, "mgit-sandboxd --version")
}

// TestIsVersionSkew_NilAndUnrelated_AreNotSkew guards the trap the ticket names:
// an absent or unrelated failure must never be read as a version mismatch.
// Refs: MGIT-136
func TestIsVersionSkew_NilAndUnrelated_AreNotSkew(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"transport_failure", errors.New("sandbox client: dial daemon: connection refused"), false},
		{"greeting_failure", errors.New("sandbox client: daemon did not greet"), false},
		{"daemon_stall", errStalledDaemon, false},
		{"guest_not_serving", model.ErrGuestNotServing, false},
		{"skew_by_identity", errVersionSkew, true},
		{"skew_by_text", errors.New("x: " + model.ErrSandboxVersionSkew.Error()), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isVersionSkew(tt.err))
		})
	}
}
