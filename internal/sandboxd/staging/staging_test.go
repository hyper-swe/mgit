// Package staging tests verify the ONE security-critical SEC-03 delivery-tree
// builder shared by every sandbox backend: worktree files + the private .mgit
// only, in-worktree stores dropped, escaping symlinks rejected fail-closed.
// Refs: SEC-03, FR-17.3, F-A/NEW-2
package staging

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// privateStoreWith creates a fake private .mgit store dir with one marker file.
func privateStoreWith(t *testing.T) string {
	t.Helper()
	priv := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(priv, "objects"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(priv, "HEAD"), []byte("ref: refs/heads/task/x\n"), 0o600))
	return priv
}

// have returns the set of staging-relative (slash) paths present under dir.
func have(t *testing.T, dir string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	require.NoError(t, filepath.WalkDir(dir, func(path string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		if rel != "." {
			out[filepath.ToSlash(rel)] = true
		}
		return nil
	}))
	return out
}

// TestBuild_PacksWorktreePlusPrivateMgit verifies the SEC-03 delivery tree:
// worktree files (nested too) plus the private store at .mgit, with any
// in-worktree store dropped (finding F-A). Refs: SEC-03
func TestBuild_PacksWorktreePlusPrivateMgit(t *testing.T) {
	wt := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(wt, "main.go"), []byte("package main"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(wt, "pkg"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(wt, "pkg", "x.go"), []byte("package pkg"), 0o600))
	// In-worktree stores that must NOT be packed (a clone's history, F-A).
	require.NoError(t, os.MkdirAll(filepath.Join(wt, ".mgit", "objects"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(wt, ".mgit", "LEAK"), []byte("host history"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(wt, ".git"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(wt, ".git", "CLONE_LEAK"), []byte("clone history"), 0o600))

	priv := privateStoreWith(t)
	stage := filepath.Join(t.TempDir(), "stage")
	require.NoError(t, Build(wt, priv, stage))

	h := have(t, stage)
	assert.True(t, h["main.go"], "worktree file packed")
	assert.True(t, h["pkg/x.go"], "nested worktree file packed")
	assert.True(t, h[".mgit/HEAD"], "private store laid in at .mgit")
	assert.True(t, h[".mgit/objects"], "private store subtree laid in")
	assert.False(t, h[".mgit/LEAK"], "the in-worktree .mgit content is NOT packed")
	assert.False(t, h[".git/CLONE_LEAK"], "the in-worktree .git (clone history) is NOT packed")

	// The private store content is the laid-in one, not the worktree's.
	got, err := os.ReadFile(filepath.Join(stage, ".mgit", "HEAD")) //nolint:gosec // test fixture path under t.TempDir()
	require.NoError(t, err)
	assert.Equal(t, "ref: refs/heads/task/x\n", string(got))
}

// TestBuild_RejectsEscapingSymlink proves an escaping worktree symlink fails
// the build CLOSED with the sentinel (finding F-A/NEW-2). Refs: SEC-03
func TestBuild_RejectsEscapingSymlink(t *testing.T) {
	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "secret"), []byte("host secret"), 0o600))

	cases := map[string]string{
		"absolute_outside": filepath.Join(outside, "secret"),
		"relative_dotdot":  filepath.Join("..", filepath.Base(outside), "secret"),
	}
	for name, target := range cases {
		t.Run(name, func(t *testing.T) {
			wt := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(wt, "ok.txt"), []byte("ok"), 0o600))
			require.NoError(t, os.Symlink(target, filepath.Join(wt, "escape")))

			stage := filepath.Join(t.TempDir(), "stage")
			err := Build(wt, privateStoreWith(t), stage)
			require.ErrorIs(t, err, ErrSymlinkEscape,
				"an escaping symlink must fail closed with the sentinel")
		})
	}
}

// TestBuild_PreservesInWorktreeSymlink verifies an in-worktree symlink (target
// stays inside) is preserved verbatim, not rejected.
func TestBuild_PreservesInWorktreeSymlink(t *testing.T) {
	wt := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(wt, "real.txt"), []byte("x"), 0o600))
	require.NoError(t, os.Symlink("real.txt", filepath.Join(wt, "link.txt"))) // relative, in-tree
	require.NoError(t, os.MkdirAll(filepath.Join(wt, "sub"), 0o750))
	require.NoError(t, os.Symlink(filepath.Join(wt, "real.txt"), filepath.Join(wt, "sub", "abs.txt"))) // absolute, in-tree

	stage := filepath.Join(t.TempDir(), "stage")
	require.NoError(t, Build(wt, privateStoreWith(t), stage))

	h := have(t, stage)
	assert.True(t, h["link.txt"], "an in-worktree relative symlink is preserved")
	assert.True(t, h["sub/abs.txt"], "an in-worktree absolute symlink is preserved")
	fi, err := os.Lstat(filepath.Join(stage, "link.txt"))
	require.NoError(t, err)
	assert.NotZero(t, fi.Mode()&os.ModeSymlink, "preserved as a symlink, not a copy")
}

// TestBuild_EmptyWorktree verifies a worktree with no files still yields a tree
// carrying the private store (boundary: nothing to pack but the store).
func TestBuild_EmptyWorktree(t *testing.T) {
	wt := t.TempDir()
	stage := filepath.Join(t.TempDir(), "stage")
	require.NoError(t, Build(wt, privateStoreWith(t), stage))
	assert.True(t, have(t, stage)[".mgit/HEAD"], "private store is laid in even for an empty worktree")
}

// TestBuild_GuestStoreName documents the guest store target stays .mgit
// (ADR-001 amendment / MGIT-14), so the firecracker/vzf/container deliveries
// agree on where the private store lands.
func TestBuild_GuestStoreName(t *testing.T) {
	assert.Equal(t, ".mgit", GuestStoreName)
}
