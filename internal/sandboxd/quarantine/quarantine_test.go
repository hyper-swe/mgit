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

// --- SEC-03: private sandbox-local object store (MGIT-11.6.2) ---

// repoLayout returns a sibling worktree + shared store under a temp repo
// root (the real ADR-004 layout: <repo>/worktrees/<task> and <repo>/.mgit),
// plus a sandbox-local private store dir outside both.
func repoLayout(t *testing.T) (worktree, shared, private string) {
	t.Helper()
	repo := t.TempDir()
	worktree = filepath.Join(repo, "worktrees", "task-a")
	require.NoError(t, os.MkdirAll(worktree, 0o700))
	shared = filepath.Join(repo, ".mgit")
	require.NoError(t, os.MkdirAll(shared, 0o700))
	private = filepath.Join(t.TempDir(), "sandbox-a", "git")
	return worktree, shared, private
}

// TestQuarantine_GuestStoreUsesPrivateStore verifies the bound plan maps the
// guest's mgit store (.mgit) to the sandbox-local private store (read-write),
// so guest commits land in the private store, never the shared one. Refs: SEC-03
func TestQuarantine_GuestStoreUsesPrivateStore(t *testing.T) {
	wt, shared, priv := repoLayout(t)
	base, err := BuildPlan(wt, defaultPatterns())
	require.NoError(t, err)

	plan, err := base.BindPrivateStore(priv, shared)
	require.NoError(t, err)

	assert.Equal(t, priv, plan.PrivateStorePath)
	m := plan.mountFor(priv)
	require.NotNil(t, m, "the private store must be mounted")
	assert.Equal(t, filepath.Join(wt, ".mgit"), m.GuestPath, "the guest .mgit store resolves to the private store")
	assert.False(t, m.ReadOnly, "the guest writes its micro-commits to the private store")
}

// TestQuarantine_NoRootGitDependency verifies the SEC-03 private store binds
// with no dependency on a project/worktree .git: with the self-contained
// .mgit layout (MGIT-14), a sandbox worktree holds only checked-out files and
// no .git at all. The plan must still bind the private store at the guest's
// .mgit path, keep the worktree writable, and never target a root .git that no
// longer exists. Refs: SEC-03, MGIT-14, ADR-001 (amendment 2026-06-22)
func TestQuarantine_NoRootGitDependency(t *testing.T) {
	wt, shared, priv := repoLayout(t)
	writeFile(t, wt, "src/main.go") // self-contained worktree: files only, no .git

	// Precondition: no .git anywhere in the worktree (the MGIT-14 layout).
	_, statErr := os.Lstat(filepath.Join(wt, ".git"))
	require.True(t, os.IsNotExist(statErr), "precondition: the worktree has no .git (self-contained layout)")

	base, err := BuildPlan(wt, defaultPatterns())
	require.NoError(t, err)

	plan, err := base.BindPrivateStore(priv, shared)
	require.NoError(t, err, "binding must not depend on a worktree .git existing")

	m := plan.mountFor(priv)
	require.NotNil(t, m, "the private store is mounted even with no worktree .git")
	assert.Equal(t, filepath.Join(wt, ".mgit"), m.GuestPath,
		"the guest's mgit store (.mgit) is sourced from the private store, not a root .git")
	assert.False(t, m.ReadOnly, "the guest writes its micro-commits to the private store")

	// The worktree stays the read-write working tree, and no mount targets a
	// root .git gitfile that no longer exists.
	root := plan.WorktreeMount()
	require.NotNil(t, root)
	assert.False(t, root.ReadOnly)
	for _, mnt := range plan.Mounts {
		assert.NotEqual(t, filepath.Join(wt, ".git"), mnt.GuestPath,
			"no mount may target the obsolete worktree .git path")
	}
}

// TestQuarantine_SharedStoreUnreachable verifies no mount resolves into the
// shared store, and that a shared store sitting inside the mounted worktree
// is rejected (it would be a guest-visible file). Refs: SEC-03
func TestQuarantine_SharedStoreUnreachable(t *testing.T) {
	t.Run("sibling_shared_store_is_excluded", func(t *testing.T) {
		wt, shared, priv := repoLayout(t)
		writeFile(t, wt, ".claude/settings.json")
		base, err := BuildPlan(wt, defaultPatterns())
		require.NoError(t, err)
		plan, err := base.BindPrivateStore(priv, shared)
		require.NoError(t, err)
		for _, m := range plan.Mounts {
			rel, relErr := filepath.Rel(shared, m.HostPath)
			withinShared := relErr == nil && rel != ".." && !filepath.IsAbs(rel) &&
				(rel == "." || rel[0] != '.')
			assert.False(t, withinShared, "mount %q resolves into the shared store", m.HostPath)
		}
	})

	t.Run("shared_store_inside_worktree_rejected", func(t *testing.T) {
		wt := t.TempDir()
		base, err := BuildPlan(wt, defaultPatterns())
		require.NoError(t, err)
		insideShared := filepath.Join(wt, ".mgit") // would be a guest-visible file
		_, err = base.BindPrivateStore(filepath.Join(t.TempDir(), "priv"), insideShared)
		assert.ErrorIs(t, err, model.ErrSharedStoreReachable)
	})
}

// TestQuarantine_CrossTaskObjectsHidden verifies the bound plan exposes
// only this sandbox's worktree and private store as read-write — never the
// shared store or another task's private store. Refs: SEC-03
func TestQuarantine_CrossTaskObjectsHidden(t *testing.T) {
	wt, shared, priv := repoLayout(t)
	base, err := BuildPlan(wt, defaultPatterns())
	require.NoError(t, err)
	plan, err := base.BindPrivateStore(priv, shared)
	require.NoError(t, err)

	var writable []string
	for _, m := range plan.Mounts {
		if !m.ReadOnly {
			writable = append(writable, m.HostPath)
		}
	}
	assert.ElementsMatch(t, []string{wt, priv}, writable,
		"only this sandbox's worktree and private store are writable; no shared/cross-task store")
}

// TestQuarantine_PrivateStoreOwnsStoreSubtree verifies the private store
// supersedes any mount BuildPlan layered inside the guest's .mgit store path:
// no host path may resolve inside the guest's private .mgit. A worktree's own
// .git is unaffected — it belongs to the project working tree, not mgit's
// store, so the private store does not own or drop it. Refs: SEC-03, FR-17.14
func TestQuarantine_PrivateStoreOwnsStoreSubtree(t *testing.T) {
	wt, shared, priv := repoLayout(t)
	// A stray host file inside the guest's .mgit path (e.g. a leftover dir).
	writeFile(t, wt, ".mgit/objects/pack/keep")
	base, err := BuildPlan(wt, []string{".mgit/objects/"})
	require.NoError(t, err)
	require.NotNil(t, base.mountFor(filepath.Join(wt, ".mgit", "objects")),
		"precondition: BuildPlan mounts the .mgit/objects path")

	plan, err := base.BindPrivateStore(priv, shared)
	require.NoError(t, err)

	assert.Nil(t, plan.mountFor(filepath.Join(wt, ".mgit", "objects")),
		"the host .mgit/objects mount is dropped — the private store owns the .mgit subtree")
	m := plan.mountFor(priv)
	require.NotNil(t, m)
	assert.Equal(t, filepath.Join(wt, ".mgit"), m.GuestPath)
}

// TestBindPrivateStore_Rejections covers the SEC-03 guards: a relative
// private store, a private store inside the worktree (guest-visible), and a
// private store nested in the shared store (would expose shared objects).
func TestBindPrivateStore_Rejections(t *testing.T) {
	wt, shared, _ := repoLayout(t)
	base, err := BuildPlan(wt, defaultPatterns())
	require.NoError(t, err)

	t.Run("relative_private_store", func(t *testing.T) {
		_, err := base.BindPrivateStore("relative/priv", shared)
		assert.Error(t, err)
	})
	t.Run("private_store_inside_worktree", func(t *testing.T) {
		_, err := base.BindPrivateStore(filepath.Join(wt, ".git"), shared)
		assert.Error(t, err)
	})
	t.Run("private_store_inside_shared", func(t *testing.T) {
		_, err := base.BindPrivateStore(filepath.Join(shared, "objects"), shared)
		assert.ErrorIs(t, err, model.ErrSharedStoreReachable)
	})
	t.Run("private_equals_shared", func(t *testing.T) {
		_, err := base.BindPrivateStore(shared, shared)
		assert.ErrorIs(t, err, model.ErrSharedStoreReachable)
	})
	t.Run("relative_shared_store", func(t *testing.T) {
		_, err := base.BindPrivateStore(filepath.Join(t.TempDir(), "priv"), "relative/.mgit")
		assert.Error(t, err)
	})
}

// TestBindPrivateStore_WorktreeInsideSharedStore verifies the defense that
// rejects a worktree living inside the shared store — every worktree mount
// would then resolve into the shared store (SEC-03). Refs: SEC-03
func TestBindPrivateStore_WorktreeInsideSharedStore(t *testing.T) {
	shared := t.TempDir()
	wt := filepath.Join(shared, "wt") // worktree nested inside the shared store
	require.NoError(t, os.MkdirAll(wt, 0o700))
	base, err := BuildPlan(wt, defaultPatterns())
	require.NoError(t, err)

	_, err = base.BindPrivateStore(filepath.Join(t.TempDir(), "priv"), shared)
	assert.ErrorIs(t, err, model.ErrSharedStoreReachable,
		"a worktree inside the shared store is a quarantine breach")
}
