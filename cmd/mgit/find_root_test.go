package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFindRepoRoot_WalksUpToNearestMgit verifies mgit locates its store by
// walking up the directory tree (like git finds .git), so commands run from a
// subdirectory resolve to the repo root rather than failing. Refs: MGIT-24
func TestFindRepoRoot_WalksUpToNearestMgit(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".mgit"), 0o750))
	deep := filepath.Join(root, "sub", "deep")
	require.NoError(t, os.MkdirAll(deep, 0o750))

	// EvalSymlinks because t.TempDir on macOS is under /var -> /private/var.
	wantRoot, err := filepath.EvalSymlinks(root)
	require.NoError(t, err)

	for _, start := range []string{root, filepath.Join(root, "sub"), deep} {
		got, err := findRepoRoot(start)
		require.NoError(t, err, "must find .mgit walking up from %s", start)
		gotEval, err := filepath.EvalSymlinks(got)
		require.NoError(t, err)
		assert.Equal(t, wantRoot, gotEval, "must resolve to the dir containing .mgit")
	}
}

// TestFindRepoRoot_NoMgitAnywhere returns an error when no .mgit exists up to
// the filesystem root. Refs: MGIT-24
func TestFindRepoRoot_NoMgitAnywhere(t *testing.T) {
	start := filepath.Join(t.TempDir(), "a", "b")
	require.NoError(t, os.MkdirAll(start, 0o750))
	_, err := findRepoRoot(start)
	assert.Error(t, err, "no .mgit up to filesystem root must be an error")
}

// TestFindRepoRoot_IgnoresMgitFile verifies a plain FILE named .mgit does not
// satisfy discovery — only a .mgit directory (the store) counts. Refs: MGIT-24
func TestFindRepoRoot_IgnoresMgitFile(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, ".mgit"), []byte("x"), 0o600))
	_, err := findRepoRoot(root)
	assert.Error(t, err, "a .mgit file (not a dir) must not satisfy discovery")
}
