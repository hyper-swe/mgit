package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/hyper-swe/mgit/internal/model"
)

func renderGuestFailure(err error) (guestFailure, string) {
	f := classifyGuestFailure(err, entitlementUnknown)
	var b bytes.Buffer
	writeGuestFailure(&b, &model.SandboxInfo{TaskID: "REG-2"}, f)
	return f, b.String()
}

// A launch that fails while the image is being resolved has not started a guest:
// nothing was capped, nothing ran, and the memory advice cannot help. The image
// store's own errors can carry words that also mark a lost guest ("connection
// refused" from an unreachable registry, "EOF" from a cut download), so the
// phase is decided by the launch step that failed, ahead of the lost-guest
// markers. Refs: MGIT-108, MGIT-118
func TestGuestFailure_APreBootLaunchFailureCarriesItsOwnCauseAndNoMemoryAdvice(t *testing.T) {
	for _, tt := range []struct{ name, msg string }{
		{"trust root", `sandbox exec: sandbox ensure-running: libkrun launch: resolve image "base@sha256:abc": image store unavailable: images: load trust root (run GenerateTrustRoot first): missing or invalid key`},
		{"registry unreachable", `sandbox exec: sandbox ensure-running: libkrun launch: resolve image "base@sha256:abc": pull: dial tcp 10.0.0.1:443: connect: connection refused`},
		{"download cut", `sandbox exec: sandbox ensure-running: firecracker launch: resolve image "base@sha256:abc": read layer: unexpected EOF`},
		{"no backend", `sandbox exec: sandbox ensure-running: ` + model.ErrSandboxBackendUnavailable.Error()},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f, out := renderGuestFailure(errors.New(tt.msg))
			assert.Equal(t, phaseNotLaunched, f.phase)
			assert.Contains(t, out, "no guest was started")
			for _, advice := range []string{"stopped answering", "--memory-mb", "memory exhaustion", "capped at"} {
				assert.NotContains(t, out, advice, "the memory advice must not appear for a failure before any guest existed")
			}
			assert.True(t, strings.Contains(out, "REG-2"))
		})
	}
}

// The other side: a guest that was serving and then dropped the connection
// still gets the cap advisory. Refs: MGIT-108, MGIT-95
func TestGuestFailure_AGuestLostMidCommandStillGetsTheCapAdvisory(t *testing.T) {
	f, out := renderGuestFailure(errors.New("sandbox exec: read frame: EOF"))
	assert.Equal(t, phaseLostServing, f.phase)
	assert.Contains(t, out, "stopped answering mid-command")
}
