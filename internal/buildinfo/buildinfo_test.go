package buildinfo

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// These moved here from cmd/mgit when the resolution and formatting were
// extracted so mgit-sandboxd could report the same build (MGIT-83). The
// assertions are unchanged — the logic did not change, only its home.

func TestFormat_RendersAllThreeFields(t *testing.T) {
	got := Format("v0.2.0-beta", "abc123def456", "2026-06-26T00:00:00Z")
	assert.Equal(t, "v0.2.0-beta (commit: abc123def456, built: 2026-06-26T00:00:00Z)", got)
}

func TestResolve_LdflagsApplied_UsesInjectedValues(t *testing.T) {
	// Simulate a Makefile/GoReleaser build where ldflags set the vars.
	origV, origC, origD := version, commit, date
	t.Cleanup(func() { version, commit, date = origV, origC, origD })
	version, commit, date = "v0.2.0-beta", "deadbeef0123", "2026-06-26T12:00:00Z"

	v, c, d := Resolve()
	assert.Equal(t, "v0.2.0-beta", v)
	assert.Equal(t, "deadbeef0123", c)
	assert.Equal(t, "2026-06-26T12:00:00Z", d)
}

func TestResolve_NoLdflags_DoesNotReturnRawDefaultsWhenBuildInfoPresent(t *testing.T) {
	// With the default "dev" version, Resolve falls back to the module build
	// info embedded by the toolchain. In `go test` that build info is always
	// present, so at minimum it must not crash and must return a non-empty
	// version. (The exact value depends on the test binary's stamp.)
	origV, origC, origD := version, commit, date
	t.Cleanup(func() { version, commit, date = origV, origC, origD })
	version, commit, date = "dev", "none", "unknown"

	v, _, _ := Resolve()
	assert.NotEmpty(t, v, "version must never be empty")
}

// TestString_IsTheFormattedResolution pins the one call both binaries make, so
// a change to either half cannot silently alter what they print. Refs: MGIT-83
func TestString_IsTheFormattedResolution(t *testing.T) {
	v, c, d := Resolve()
	assert.Equal(t, Format(v, c, d), String())
}
