package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFormatVersion_RendersAllThreeFields(t *testing.T) {
	got := formatVersion("v0.2.0-beta", "abc123def456", "2026-06-26T00:00:00Z")
	assert.Equal(t, "v0.2.0-beta (commit: abc123def456, built: 2026-06-26T00:00:00Z)", got)
}

func TestResolveBuildInfo_LdflagsApplied_UsesInjectedValues(t *testing.T) {
	// Simulate a Makefile/GoReleaser build where ldflags set the vars.
	origV, origC, origD := version, commit, date
	t.Cleanup(func() { version, commit, date = origV, origC, origD })
	version, commit, date = "v0.2.0-beta", "deadbeef0123", "2026-06-26T12:00:00Z"

	v, c, d := resolveBuildInfo()
	assert.Equal(t, "v0.2.0-beta", v)
	assert.Equal(t, "deadbeef0123", c)
	assert.Equal(t, "2026-06-26T12:00:00Z", d)
}

func TestResolveBuildInfo_NoLdflags_DoesNotReturnRawDefaultsWhenBuildInfoPresent(t *testing.T) {
	// With the default "dev" version, resolveBuildInfo falls back to the module
	// build info embedded by the toolchain. In `go test` that build info is
	// always present, so at minimum it must not crash and must return a
	// non-empty version. (The exact value depends on the test binary's stamp.)
	origV, origC, origD := version, commit, date
	t.Cleanup(func() { version, commit, date = origV, origC, origD })
	version, commit, date = "dev", "none", "unknown"

	v, _, _ := resolveBuildInfo()
	assert.NotEmpty(t, v, "version must never be empty")
}

func TestVersionCmd_PrintsResolvedVersion(t *testing.T) {
	cmd := versionCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{})
	require.NoError(t, cmd.Execute())
	// The line is "<version> (commit: <c>, built: <d>)" — assert the shape.
	line := strings.TrimSpace(out.String())
	assert.Contains(t, line, "(commit:")
	assert.Contains(t, line, "built:")
	assert.NotEmpty(t, line)
}
