package agentadapter

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/land"
)

// fakeMgit writes a stand-in `mgit` executable that appends its argv to a
// log file, returning the binary path and the log path. It lets the shim
// tests prove a wrapper actually invokes `mgit run -- <cmd>`.
func fakeMgit(t *testing.T) (bin, logPath string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "mgit")
	logPath = filepath.Join(dir, "calls.log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + logPath + "\n"
	require.NoError(t, os.WriteFile(bin, []byte(script), 0o700)) //nolint:gosec // test-owned exec
	return bin, logPath
}

// assertShimRoutes runs the named shim in shimDir and asserts the fake
// mgit recorded a `run -- <cmd> <args>` invocation.
func assertShimRoutes(t *testing.T, shimDir, logPath, cmd string, args ...string) {
	t.Helper()
	out, err := exec.Command(filepath.Join(shimDir, cmd), args...).CombinedOutput() //nolint:gosec // test-owned shim path
	require.NoError(t, err, string(out))
	logged, err := os.ReadFile(logPath) //nolint:gosec // test-owned temp path
	require.NoError(t, err)
	want := strings.TrimSpace(strings.Join(append([]string{"run", "--", cmd}, args...), " "))
	assert.Contains(t, string(logged), want, "shim must route via `mgit run -- <cmd>`")
}

// TestAdapter_CodexRoutesViaShim verifies the Codex adapter writes an
// AGENTS.md directive plus a PATH shim that routes commands into the guest.
// Refs: MGIT-11.11.3
func TestAdapter_CodexRoutesViaShim(t *testing.T) {
	wt := t.TempDir()
	bin, logPath := fakeMgit(t)
	require.NoError(t, WriteCodexAdapter(wt, bin))

	agents, err := os.ReadFile(filepath.Join(wt, "AGENTS.md")) //nolint:gosec // test-owned temp path
	require.NoError(t, err)
	assert.Contains(t, string(agents), "microVM")
	assert.Contains(t, string(agents), ShimDir(wt), "AGENTS.md points at the shim dir")

	assertShimRoutes(t, ShimDir(wt), logPath, "npm", "install")
}

// TestAdapter_CursorRoutesViaShim verifies the Cursor adapter writes a
// rules file plus a routing shim. Refs: MGIT-11.11.3
func TestAdapter_CursorRoutesViaShim(t *testing.T) {
	wt := t.TempDir()
	bin, logPath := fakeMgit(t)
	require.NoError(t, WriteCursorAdapter(wt, bin))

	rule, err := os.ReadFile(filepath.Join(wt, ".cursor", "rules", "mgit-sandbox.mdc")) //nolint:gosec // test-owned temp path
	require.NoError(t, err)
	assert.Contains(t, string(rule), "microVM")

	assertShimRoutes(t, ShimDir(wt), logPath, "go", "test")
}

// TestAdapter_GenericPathShimRoutes verifies the generic adapter writes a
// direnv .envrc that prepends the shim dir, and the shim routes.
// Refs: MGIT-11.11.3
func TestAdapter_GenericPathShimRoutes(t *testing.T) {
	wt := t.TempDir()
	bin, logPath := fakeMgit(t)
	require.NoError(t, WriteGenericAdapter(wt, bin))

	envrc, err := os.ReadFile(filepath.Join(wt, ".envrc")) //nolint:gosec // test-owned temp path
	require.NoError(t, err)
	assert.Contains(t, string(envrc), ShimDir(wt))
	assert.Contains(t, string(envrc), "PATH")

	assertShimRoutes(t, ShimDir(wt), logPath, "make")
}

// TestAdapter_CooperativeNotice verifies every generated directive states
// the adapters are cooperative and that the hard guarantee is the land
// attestation gate. Refs: MGIT-11.11.3
func TestAdapter_CooperativeNotice(t *testing.T) {
	wt := t.TempDir()
	bin, _ := fakeMgit(t)
	require.NoError(t, WriteCodexAdapter(wt, bin))
	require.NoError(t, WriteCursorAdapter(wt, bin))

	for _, p := range []string{
		filepath.Join(wt, "AGENTS.md"),
		filepath.Join(wt, ".cursor", "rules", "mgit-sandbox.mdc"),
	} {
		b, err := os.ReadFile(p) //nolint:gosec // test-owned temp path
		require.NoError(t, err)
		body := strings.ToLower(string(b))
		assert.Contains(t, body, "cooperative", "%s states cooperative-not-enforced", p)
		assert.Contains(t, body, "require_sandbox", "%s names the enforced backstop", p)
		assert.Contains(t, body, "land", "%s explains the land block", p)
	}
}

// TestAdapter_BypassBlockedAtLandWithoutAttestation verifies the hard
// guarantee the cooperative adapters rely on: a commit produced outside
// the sandbox (no host attestation) is REFUSED at land when require_sandbox
// is on (the default) — a bypass is a blocked state, not a silent gap.
// Refs: MGIT-11.11.3, FR-17.6
func TestAdapter_BypassBlockedAtLandWithoutAttestation(t *testing.T) {
	// require_sandbox ON, a commit with NO attestation (the bypass case).
	_, err := land.EnforceRequireSandbox(context.Background(), true, "sbx-bound",
		&model.Commit{CommitID: "deadbeef", ContentHash: "abc"}, nil, nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrUnattestedCommit, "unattested host-bypass commit is blocked at land")
}

// TestInstallCooperativeAdapters_WritesAll verifies the combined installer
// drops every harness's config plus the shim dir. Refs: MGIT-11.11.3
func TestInstallCooperativeAdapters_WritesAll(t *testing.T) {
	wt := t.TempDir()
	bin, _ := fakeMgit(t)
	require.NoError(t, InstallCooperativeAdapters(wt, bin))

	for _, rel := range []string{
		"AGENTS.md",
		filepath.Join(".cursor", "rules", "mgit-sandbox.mdc"),
		".envrc",
		filepath.Join(".mgit", "shims", "npm"),
	} {
		_, err := os.Stat(filepath.Join(wt, rel))
		assert.NoError(t, err, "expected %s", rel)
	}
}

// TestInstallShims_RejectsTraversal verifies a command name with a path
// separator (which would escape the shim dir) is refused. Refs: MGIT-11.11.3
func TestInstallShims_RejectsTraversal(t *testing.T) {
	wt := t.TempDir()
	bin, _ := fakeMgit(t)
	for _, bad := range []string{"../evil", "a/b", ""} {
		assert.Error(t, InstallShims(ShimDir(wt), bin, []string{bad}), "name %q must be rejected", bad)
	}
}

// TestWriteAdapters_FailOnUnwritableWorktree verifies each writer
// propagates a failure when the worktree path is unusable (a regular
// file). Refs: MGIT-11.11.3
func TestWriteAdapters_FailOnUnwritableWorktree(t *testing.T) {
	file := filepath.Join(t.TempDir(), "afile")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o600))
	bin, _ := fakeMgit(t)
	assert.Error(t, WriteCodexAdapter(file, bin))
	assert.Error(t, WriteCursorAdapter(file, bin))
	assert.Error(t, WriteGenericAdapter(file, bin))
}

// TestShim_UsesInvokedName verifies one shared wrapper routes whatever
// command name it is invoked as. Refs: MGIT-11.11.3
func TestShim_UsesInvokedName(t *testing.T) {
	wt := t.TempDir()
	bin, logPath := fakeMgit(t)
	require.NoError(t, InstallShims(ShimDir(wt), bin, []string{"node", "python3"}))

	assertShimRoutes(t, ShimDir(wt), logPath, "node", "-v")
	assertShimRoutes(t, ShimDir(wt), logPath, "python3", "-c", "print(1)")
}
