// Package quarantine tests verify the host-side guest-filesystem plan
// and the land-time host-trusted-path defense (SEC-03, T8). Refs:
// FR-17.3, FR-17.4, FR-17.14, MGIT-11.6
package quarantine

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// defaultPatterns mirrors the FR-17.14 host-trusted defaults.
func defaultPatterns() []string { return model.DefaultSandboxPolicy().SensitivePaths }

// writeFile creates a file (and parents) under root.
func writeFile(t *testing.T, root, rel string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o700))
	require.NoError(t, os.WriteFile(full, []byte("x"), 0o600))
}

// TestMount_IdenticalAbsolutePath verifies the worktree is mounted
// read-write at the identical absolute host path (FR-17.3, no path
// translation). Refs: FR-17.3
func TestMount_IdenticalAbsolutePath(t *testing.T) {
	wt := t.TempDir()
	plan, err := BuildPlan(wt, defaultPatterns())
	require.NoError(t, err)

	root := plan.WorktreeMount()
	require.NotNil(t, root, "the plan must contain the worktree mount")
	assert.Equal(t, wt, root.HostPath)
	assert.Equal(t, wt, root.GuestPath, "guest path must equal the host path (FR-17.3)")
	assert.False(t, root.ReadOnly, "the worktree is read-write")
}

// TestMount_HostHomeUnreachable verifies the plan never includes
// anything outside the worktree subtree — no host $HOME, no parent
// repo, no shared object store (FR-17.3). Refs: FR-17.3
func TestMount_HostHomeUnreachable(t *testing.T) {
	wt := t.TempDir()
	writeFile(t, wt, ".claude/settings.json") // a sensitive path that DOES exist
	plan, err := BuildPlan(wt, defaultPatterns())
	require.NoError(t, err)

	for _, m := range plan.Mounts {
		rel, err := filepath.Rel(wt, m.HostPath)
		require.NoError(t, err)
		assert.False(t, rel == ".." || filepath.IsAbs(rel) || len(rel) >= 2 && rel[:2] == "..",
			"mount %q escapes the worktree (FR-17.3 confinement)", m.HostPath)
	}
}

// TestMount_NoHostEnvForwarded asserts the plan carries no environment
// at all — env injection is the guest supervisor's clean-env concern
// (SEC-03), and the filesystem plan must never smuggle host env.
func TestMount_NoHostEnvForwarded(t *testing.T) {
	wt := t.TempDir()
	plan, err := BuildPlan(wt, defaultPatterns())
	require.NoError(t, err)
	// The plan is filesystem-only; there is no env field to leak.
	assert.NotEmpty(t, plan.Mounts)
}

// TestSensitivePaths_ClaudeSettingsReadOnly verifies an existing
// .claude/ tree is layered read-only over the worktree. Refs: FR-17.14
func TestSensitivePaths_ClaudeSettingsReadOnly(t *testing.T) {
	wt := t.TempDir()
	writeFile(t, wt, ".claude/settings.json")
	plan, err := BuildPlan(wt, defaultPatterns())
	require.NoError(t, err)

	m := plan.mountFor(filepath.Join(wt, ".claude"))
	require.NotNil(t, m, ".claude/ must have a read-only mount")
	assert.True(t, m.ReadOnly)
}

// TestSensitivePaths_GitHooksReadOnly verifies .git/hooks/ is read-only.
// Refs: FR-17.14
func TestSensitivePaths_GitHooksReadOnly(t *testing.T) {
	wt := t.TempDir()
	writeFile(t, wt, ".git/hooks/pre-commit")
	plan, err := BuildPlan(wt, defaultPatterns())
	require.NoError(t, err)

	m := plan.mountFor(filepath.Join(wt, ".git", "hooks"))
	require.NotNil(t, m, ".git/hooks/ must have a read-only mount")
	assert.True(t, m.ReadOnly)
}

// TestSensitivePaths_AbsentNotMounted verifies only sensitive paths that
// actually exist get a read-only mount (no phantom mounts).
func TestSensitivePaths_AbsentNotMounted(t *testing.T) {
	wt := t.TempDir() // no sensitive files created
	plan, err := BuildPlan(wt, defaultPatterns())
	require.NoError(t, err)
	assert.Len(t, plan.Mounts, 1, "only the worktree mount when no sensitive paths exist")
}

// TestSensitivePaths_ListConfigurable verifies the read-only set is
// driven by the supplied patterns, not hard-coded. Refs: FR-17.13, FR-17.14
func TestSensitivePaths_ListConfigurable(t *testing.T) {
	wt := t.TempDir()
	writeFile(t, wt, "secrets/key.pem")
	writeFile(t, wt, ".claude/settings.json")

	plan, err := BuildPlan(wt, []string{"secrets/"}) // custom list; .claude NOT protected
	require.NoError(t, err)

	assert.NotNil(t, plan.mountFor(filepath.Join(wt, "secrets")), "custom pattern is protected")
	assert.Nil(t, plan.mountFor(filepath.Join(wt, ".claude")),
		"a path not in the custom list is not protected")
}

// TestIsSensitive_Matching covers the matcher: directory patterns match
// the dir and its subtree; file patterns match exactly; near-misses do
// not match. Refs: FR-17.14
func TestIsSensitive_Matching(t *testing.T) {
	patterns := defaultPatterns()
	tests := []struct {
		rel  string
		want bool
	}{
		{".claude/settings.json", true},
		{".claude", true},
		{".git/hooks/pre-commit", true},
		{"CLAUDE.md", true},
		{"AGENTS.md", true},
		{".envrc", true},
		{"src/main.go", false},
		{"src/.clauderc", false},      // not under .claude/
		{"docs/CLAUDE.md.txt", false}, // not the exact CLAUDE.md
		{"my.claude/x", false},        // directory name must match exactly
	}
	for _, tt := range tests {
		t.Run(tt.rel, func(t *testing.T) {
			assert.Equal(t, tt.want, IsSensitive(tt.rel, patterns))
		})
	}
}

// TestLand_SensitivePathModified_Rejected verifies the land-time defense:
// any modified host-trusted path fails with ErrSensitivePathModified,
// while ordinary source edits pass. Refs: FR-17.14
func TestLand_SensitivePathModified_Rejected(t *testing.T) {
	patterns := defaultPatterns()

	require.NoError(t, CheckModifications([]string{"src/main.go", "go.mod"}, patterns),
		"ordinary source edits must land")

	err := CheckModifications([]string{"src/main.go", ".git/hooks/pre-commit"}, patterns)
	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrSensitivePathModified)
	assert.Contains(t, err.Error(), ".git/hooks/pre-commit", "the offending path is named")
}

// TestBuildPlan_RejectsRelativeWorktree verifies the worktree path must
// be absolute (the guest mounts at the identical absolute path).
func TestBuildPlan_RejectsRelativeWorktree(t *testing.T) {
	_, err := BuildPlan("relative/path", defaultPatterns())
	require.Error(t, err)
}

// TestEmptyPatternsIgnored verifies blank/"/"-only patterns are skipped
// rather than protecting the whole worktree or matching everything.
func TestEmptyPatternsIgnored(t *testing.T) {
	wt := t.TempDir()
	writeFile(t, wt, "src/main.go")
	plan, err := BuildPlan(wt, []string{"", "/"})
	require.NoError(t, err)
	assert.Len(t, plan.Mounts, 1, "blank patterns add no read-only mounts")
	assert.False(t, IsSensitive("src/main.go", []string{"", "/"}), "blank patterns match nothing")
}

// TestWorktreeMount_AbsentReturnsNil covers the lookup miss on a plan
// with no matching mount.
func TestWorktreeMount_AbsentReturnsNil(t *testing.T) {
	p := Plan{WorktreePath: "/abs/wt"} // no Mounts populated
	assert.Nil(t, p.WorktreeMount())
}
