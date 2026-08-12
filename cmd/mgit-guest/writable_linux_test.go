//go:build linux

package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// TestDirWritable_ProbesByActionNotByMode is the property the repair depends
// on: the failure it detects (a filesystem refusing the operation) is invisible
// to a permission check, so the probe must actually create a file — and must
// leave nothing behind when it succeeds. Refs: MGIT-89
func TestDirWritable_ProbesByActionNotByMode(t *testing.T) {
	dir := t.TempDir()

	require.NoError(t, dirWritable(dir))

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries, "the probe must remove the file it created")
}

// TestDirWritable_UnwritableDir_ReturnsError covers the negative half through a
// path that cannot be created in, so the caller's branch is exercised.
func TestDirWritable_UnwritableDir_ReturnsError(t *testing.T) {
	assert.Error(t, dirWritable(filepath.Join(t.TempDir(), "no-such-dir")))
}

// TestCopyTree_PreservesKindsAndModes covers what a guest base's /etc actually
// contains. A seeded /etc that lost its symlinks or its modes would be worse
// than the unwritable one it replaces: /etc/resolv.conf is frequently a
// symlink, and a CA bundle or shadow file with the wrong mode is a security
// change, not a copy. Refs: MGIT-89
// The permissions below are the FIXTURE, not a choice: this test asserts that
// copyTree preserves a base image's real modes, so a world-readable CA bundle
// (0644) and a group-readable shadow (0640) have to be written as such. Reducing
// them to satisfy gosec would delete the property under test. The reads are of
// paths this test just created.
//
//nolint:gosec // G301,G306,G304: the modes and paths are the fixture.
func TestCopyTree_PreservesKindsAndModes(t *testing.T) {
	src, dst := t.TempDir(), filepath.Join(t.TempDir(), "seeded")
	require.NoError(t, os.MkdirAll(filepath.Join(src, "ssl", "certs"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(src, "nsswitch.conf"), []byte("hosts: files dns\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(src, "shadow"), []byte("root:!:1::::::\n"), 0o640))
	require.NoError(t, os.WriteFile(filepath.Join(src, "ssl", "certs", "ca.pem"), []byte("PEM\n"), 0o644))
	// A dangling symlink: normal in a base image, and it must survive as one.
	require.NoError(t, os.Symlink("../run/resolv.conf", filepath.Join(src, "resolv.conf")))

	require.NoError(t, copyTree(src, dst))

	got, err := os.ReadFile(filepath.Join(dst, "nsswitch.conf"))
	require.NoError(t, err)
	assert.Equal(t, "hosts: files dns\n", string(got))

	nested, err := os.ReadFile(filepath.Join(dst, "ssl", "certs", "ca.pem"))
	require.NoError(t, err)
	assert.Equal(t, "PEM\n", string(nested))

	info, err := os.Stat(filepath.Join(dst, "shadow"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o640), info.Mode().Perm(), "modes must survive the copy")

	link, err := os.Readlink(filepath.Join(dst, "resolv.conf"))
	require.NoError(t, err)
	assert.Equal(t, "../run/resolv.conf", link, "a symlink must be recreated as a symlink, dangling or not")

	dirInfo, err := os.Stat(filepath.Join(dst, "ssl", "certs"))
	require.NoError(t, err)
	assert.True(t, dirInfo.IsDir())
}

// TestCopyTree_SkipsWhatItCannotFaithfullyReproduce pins the deliberate
// omission: a node kind this copier does not handle is skipped, never guessed
// at. The rest of the tree must still arrive. Refs: MGIT-89
func TestCopyTree_SkipsWhatItCannotFaithfullyReproduce(t *testing.T) {
	src, dst := t.TempDir(), filepath.Join(t.TempDir(), "seeded")
	require.NoError(t, os.WriteFile(filepath.Join(src, "keep.conf"), []byte("k\n"), 0o600))
	if err := mkfifoForTest(filepath.Join(src, "afifo")); err != nil {
		t.Skipf("SKIP: cannot create a fifo here (%v); the skip branch is unexercised", err)
	}

	require.NoError(t, copyTree(src, dst))

	_, err := os.Stat(filepath.Join(dst, "keep.conf"))
	assert.NoError(t, err, "ordinary files must still be copied alongside a skipped node")
	_, err = os.Lstat(filepath.Join(dst, "afifo"))
	assert.True(t, os.IsNotExist(err), "an unreproducible node is omitted, not invented")
}

// TestEnsureWritableDir_AlreadyWritable_IsANoOp is the branch every backend
// whose overlay copies up normally takes: no tmpfs, no seeding, nothing
// changed. Refs: MGIT-89
func TestEnsureWritableDir_AlreadyWritable_IsANoOp(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "keep"), []byte("x"), 0o600))

	require.NoError(t, ensureWritableDir(dir, discardLogger()))

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "keep", entries[0].Name(), "a writable directory must be left exactly as it was")
}

// discardLogger is a logger whose output no test needs to read.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// mkfifoForTest creates a fifo, the stand-in for "a node kind copyTree does
// not reproduce".
func mkfifoForTest(path string) error {
	return unix.Mkfifo(path, 0o600)
}

// TestEnsureWritableDir_AbsentDir_IsCreated covers the minimal guest base: a
// root that ships no /etc at all. Creating it is correct and sufficient — a new
// directory lives in the overlay's upper, which is writable even where copy-up
// is not — and treating its absence as a failure would kill the boot of every
// workload-only guest. Refs: MGIT-89
func TestEnsureWritableDir_AbsentDir_IsCreated(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "etc")

	require.NoError(t, ensureWritableDir(dir, discardLogger()))

	info, err := os.Stat(dir)
	require.NoError(t, err)
	assert.True(t, info.IsDir())
	assert.NoError(t, dirWritable(dir), "the created directory must be writable")
}
