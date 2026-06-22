package agentadapter

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGenDoc_StatesMicroVMAndPath verifies the section tells the agent
// commands run in a microVM mounted at the identical path. Refs: MGIT-11.11.2
func TestGenDoc_StatesMicroVMAndPath(t *testing.T) {
	wt := filepath.FromSlash("/repo/wt")
	s := RenderClaudeMdSection(SandboxEnv{WorktreePath: wt, NetworkMode: "none"})

	assert.Contains(t, s, "microVM")
	assert.Contains(t, s, "identical path")
	assert.Contains(t, s, wt, "the worktree path is stated")
	assert.Contains(t, s, "without asking", "agent is told to run freely")
}

// TestGenDoc_StatesNetworkPosture verifies the section states the network
// mode and, for allowlist, the permitted destinations. Refs: MGIT-11.11.2
func TestGenDoc_StatesNetworkPosture(t *testing.T) {
	t.Run("none", func(t *testing.T) {
		s := RenderClaudeMdSection(SandboxEnv{NetworkMode: "none"})
		assert.Contains(t, s, "No network")
	})
	t.Run("allowlist", func(t *testing.T) {
		s := RenderClaudeMdSection(SandboxEnv{NetworkMode: "allowlist", Allowlist: []string{"registry.npmjs.org", "github.com"}})
		assert.Contains(t, strings.ToLower(s), "allowlist")
		assert.Contains(t, s, "registry.npmjs.org")
		assert.Contains(t, s, "github.com")
	})
	t.Run("open", func(t *testing.T) {
		s := RenderClaudeMdSection(SandboxEnv{NetworkMode: "open"})
		assert.Contains(t, s, "Open network")
	})
}

// TestGenDoc_RemedyGuidance verifies the section explains the
// machine-readable MGIT-EGRESS-DENIED error and its remedy. Refs: MGIT-11.11.2
func TestGenDoc_RemedyGuidance(t *testing.T) {
	s := RenderClaudeMdSection(SandboxEnv{NetworkMode: "allowlist"})
	assert.Contains(t, s, "MGIT-EGRESS-DENIED")
	assert.Contains(t, s, "remedy=")
	assert.Contains(t, s, "mgit sandbox policy request --egress")
}

// TestGenDoc_NoSecrets verifies the generator emits only the posture it is
// given and never reads ambient host secrets. Refs: MGIT-11.11.2
func TestGenDoc_NoSecrets(t *testing.T) {
	t.Setenv("AWS_SECRET_ACCESS_KEY", "super-secret-value")
	s := RenderClaudeMdSection(SandboxEnv{WorktreePath: "/repo/wt", NetworkMode: "none"})
	assert.NotContains(t, s, "super-secret-value")
	assert.NotContains(t, s, "AWS_SECRET_ACCESS_KEY")
}

// TestGenDoc_RegeneratesOnPolicyChange verifies upserting with a new
// network posture replaces the prior generated block in place, leaving a
// single block and preserving surrounding content. Refs: MGIT-11.11.2
func TestGenDoc_RegeneratesOnPolicyChange(t *testing.T) {
	wt := t.TempDir()
	path := filepath.Join(wt, "CLAUDE.md")
	require.NoError(t, os.WriteFile(path, []byte("# My Project\n\nUser notes here.\n"), 0o600))

	require.NoError(t, UpsertClaudeMd(wt, SandboxEnv{WorktreePath: wt, NetworkMode: "none"}))
	require.NoError(t, UpsertClaudeMd(wt, SandboxEnv{WorktreePath: wt, NetworkMode: "allowlist", Allowlist: []string{"github.com"}}))

	b, err := os.ReadFile(path) //nolint:gosec // test-owned temp path
	require.NoError(t, err)
	body := string(b)

	assert.Contains(t, body, "# My Project", "user content preserved")
	assert.Contains(t, body, "User notes here.", "user content preserved")
	assert.Contains(t, body, "github.com", "new posture reflected")
	assert.NotContains(t, body, "No network", "stale 'none' posture replaced")
	assert.Equal(t, 1, strings.Count(body, claudeMdBeginMarker), "exactly one generated block")
	assert.Equal(t, 1, strings.Count(body, claudeMdEndMarker), "exactly one generated block")
}

// TestUpsertClaudeMd_CreatesWhenAbsent verifies upsert creates CLAUDE.md
// when none exists. Refs: MGIT-11.11.2
func TestUpsertClaudeMd_CreatesWhenAbsent(t *testing.T) {
	wt := t.TempDir()
	require.NoError(t, UpsertClaudeMd(wt, SandboxEnv{WorktreePath: wt, NetworkMode: "none"}))

	b, err := os.ReadFile(filepath.Join(wt, "CLAUDE.md")) //nolint:gosec // test-owned temp path
	require.NoError(t, err)
	assert.Contains(t, string(b), "microVM")
}

// TestUpsertClaudeMd_ReadError verifies a CLAUDE.md that is not a regular
// file (here, a directory) surfaces an error rather than clobbering it.
// Refs: MGIT-11.11.2
func TestUpsertClaudeMd_ReadError(t *testing.T) {
	wt := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(wt, "CLAUDE.md"), 0o700))
	assert.Error(t, UpsertClaudeMd(wt, SandboxEnv{WorktreePath: wt, NetworkMode: "none"}))
}

// TestUpsertClaudeMd_WriteError verifies an unwritable worktree path
// surfaces a write error. Refs: MGIT-11.11.2
func TestUpsertClaudeMd_WriteError(t *testing.T) {
	// worktreePath is a regular file, so <path>/CLAUDE.md cannot be written.
	file := filepath.Join(t.TempDir(), "afile")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o600))
	assert.Error(t, UpsertClaudeMd(file, SandboxEnv{WorktreePath: file, NetworkMode: "none"}))
}
