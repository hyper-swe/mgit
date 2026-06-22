package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// TestWriteSandboxEnvDoc_WritesPosture verifies launch (re)generates the
// worktree CLAUDE.md env section with the sandbox's network posture.
// Refs: MGIT-11.11.2
func TestWriteSandboxEnvDoc_WritesPosture(t *testing.T) {
	wt := t.TempDir()
	var warn bytes.Buffer
	writeSandboxEnvDoc(&warn, &model.SandboxInfo{
		WorktreePath: wt, NetworkMode: model.NetworkModeAllowlist, NetworkAllowlist: []string{"github.com"},
	})

	b, err := os.ReadFile(filepath.Join(wt, "CLAUDE.md")) //nolint:gosec // test-owned temp path
	require.NoError(t, err)
	assert.Contains(t, string(b), "microVM")
	assert.Contains(t, string(b), "github.com")
	assert.Empty(t, warn.String())
}

// TestWriteSandboxEnvDoc_NilOrEmpty_NoOp verifies a nil info or empty
// worktree path writes nothing and does not panic. Refs: MGIT-11.11.2
func TestWriteSandboxEnvDoc_NilOrEmpty_NoOp(t *testing.T) {
	var warn bytes.Buffer
	writeSandboxEnvDoc(&warn, nil)
	writeSandboxEnvDoc(&warn, &model.SandboxInfo{WorktreePath: ""})
	assert.Empty(t, warn.String())
}
