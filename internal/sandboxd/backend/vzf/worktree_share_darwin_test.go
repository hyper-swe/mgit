//go:build darwin && cgo

// SEC-03 staged-share tests for vzf: vzf shares a LIVE host dir over virtiofs,
// so to deliver the quarantine it shares a STAGED copy of the worktree
// (worktree files + the private .mgit, in-worktree stores excluded, escaping
// symlinks rejected) rather than the live worktree when a private store is
// provisioned. These assert the staged TREE the share is built from, since the
// virtiofs source path is internal to the vz device. Refs: SEC-03, MGIT-11.6.9
package vzf

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/sandboxd/backend/microvm"
	"github.com/hyper-swe/mgit/internal/sandboxd/staging"
)

// shareConfig builds a VMConfig whose overlay sits in a sandbox state dir, so
// the staging tree lands beside it (cleaned by teardown's RemoveAll).
func shareConfig(t *testing.T, worktree, privateStore string) microvm.VMConfig {
	t.Helper()
	stateDir := t.TempDir()
	overlay := filepath.Join(stateDir, "overlay.img")
	require.NoError(t, os.WriteFile(overlay, make([]byte, 1024), 0o600))
	return microvm.VMConfig{
		WorktreePath:     worktree,
		WorktreeTag:      "work",
		OverlayPath:      overlay,
		PrivateStorePath: privateStore,
	}
}

// privateStoreWith creates a fake private .mgit store dir with a marker file.
func privateStoreWith(t *testing.T) string {
	t.Helper()
	priv := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(priv, "objects"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(priv, "HEAD"), []byte("ref: refs/heads/task/x\n"), 0o600))
	return priv
}

// stagingDirFor returns where worktreeShare stages, given a config's overlay.
func stagingDirFor(cfg microvm.VMConfig) string {
	return filepath.Join(filepath.Dir(cfg.OverlayPath), stagingDirName)
}

// TestWorktreeShare_PrivateStore_SharesStagedTree proves that with a private
// store wired, the share is built from a staged tree carrying the worktree
// files + the private .mgit, with the in-worktree store excluded. Refs: SEC-03
func TestWorktreeShare_PrivateStore_SharesStagedTree(t *testing.T) {
	wt := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(wt, "main.go"), []byte("package main"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(wt, ".mgit"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(wt, ".mgit", "LEAK"), []byte("host history"), 0o600))

	cfg := shareConfig(t, wt, privateStoreWith(t))
	share, err := worktreeShare(cfg)
	require.NoError(t, err)
	assert.NotNil(t, share, "a virtiofs share device is constructed")

	stage := stagingDirFor(cfg)
	assert.FileExists(t, filepath.Join(stage, "main.go"), "worktree file staged")
	assert.FileExists(t, filepath.Join(stage, staging.GuestStoreName, "HEAD"), "private store laid in at .mgit")
	assert.NoFileExists(t, filepath.Join(stage, ".mgit", "LEAK"),
		"the live worktree's own store content is never staged")
}

// TestWorktreeShare_PrivateStore_RejectsEscapingSymlink proves an escaping
// worktree symlink fails the share build CLOSED with the staging sentinel — no
// virtiofs device is built against an unquarantined worktree. Refs: SEC-03, F-A/NEW-2
func TestWorktreeShare_PrivateStore_RejectsEscapingSymlink(t *testing.T) {
	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "secret"), []byte("host secret"), 0o600))
	wt := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(wt, "ok.txt"), []byte("ok"), 0o600))
	require.NoError(t, os.Symlink(filepath.Join(outside, "secret"), filepath.Join(wt, "escape")))

	cfg := shareConfig(t, wt, privateStoreWith(t))
	_, err := worktreeShare(cfg)
	require.ErrorIs(t, err, staging.ErrSymlinkEscape,
		"the share build must fail CLOSED with the symlink-escape sentinel")
}

// TestWorktreeShare_NoPrivateStore_SharesLiveWorktree proves the pre-SEC-03
// path: with no private store, the live worktree is shared and nothing is
// staged (the documented direct/test path).
func TestWorktreeShare_NoPrivateStore_SharesLiveWorktree(t *testing.T) {
	wt := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(wt, "main.go"), []byte("package main"), 0o600))

	cfg := shareConfig(t, wt, "")
	share, err := worktreeShare(cfg)
	require.NoError(t, err)
	assert.NotNil(t, share)
	assert.NoDirExists(t, stagingDirFor(cfg), "no staging tree when no private store is wired")
}
