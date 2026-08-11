package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/buildinfo"
)

// The resolution and formatting tests moved to internal/buildinfo when that
// logic was extracted for mgit-sandboxd to share (MGIT-83). What remains here
// is what is specific to THIS binary: that the command surface prints it.

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

// TestVersionString_ComesFromTheSharedBuildInfo is what keeps `mgit --version`
// and `mgit-sandboxd --version` reporting one build: both render
// buildinfo.String(), so the release checklist's smoke step can compare them
// and a difference means the archive was assembled from two builds.
// Refs: MGIT-83, MGIT-64
func TestVersionString_ComesFromTheSharedBuildInfo(t *testing.T) {
	assert.Equal(t, buildinfo.String(), versionString())
}
