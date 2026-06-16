package hostkey

import (
	"crypto/ed25519"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFingerprint_StableHex(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	fp := Fingerprint(pub)
	assert.Len(t, fp, 64, "SHA-256 hex is 64 chars")
	assert.Equal(t, fp, Fingerprint(pub), "deterministic")

	other, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	assert.NotEqual(t, fp, Fingerprint(other), "distinct keys, distinct fingerprints")
}

func TestWriteFileAtomic_RoundTripsOwnerOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "key")
	require.NoError(t, WriteFileAtomic(path, []byte("secret")))

	got, err := os.ReadFile(path) //nolint:gosec // test path
	require.NoError(t, err)
	assert.Equal(t, "secret", string(got))
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "owner-only without an explicit chmod")
	}
	// No temp residue left behind.
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, entries, 1, "only the final file remains; the temp file was renamed away")
}

func TestWriteFileAtomic_CreateTempError(t *testing.T) {
	if os.Geteuid() == 0 || runtime.GOOS == "windows" {
		t.Skip("requires POSIX dir permissions and non-root")
	}
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0o500))       //nolint:gosec // intentional: unwritable dir
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) //nolint:gosec // restore for cleanup
	assert.Error(t, WriteFileAtomic(filepath.Join(dir, "key"), []byte("x")),
		"an unwritable directory must surface, not silently drop the write")
}

func TestWriteFileAtomic_RenameError(t *testing.T) {
	dir := t.TempDir()
	// Occupy the destination with a directory so the rename cannot complete.
	dest := filepath.Join(dir, "key")
	require.NoError(t, os.MkdirAll(dest, 0o700))
	assert.Error(t, WriteFileAtomic(dest, []byte("x")))
}

// failTemp is a tempFile whose Write and/or Close fail, to exercise the
// fail-closed key-write paths.
type failTemp struct {
	name     string
	writeErr error
	closeErr error
}

func (f *failTemp) Write(p []byte) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	return len(p), nil
}
func (f *failTemp) Close() error { return f.closeErr }
func (f *failTemp) Name() string { return f.name }

// TestWriteFileAtomic_WriteAndCloseErrorsSurface verifies a failed write
// or close fails closed — a security-critical key must never be reported
// written when the bytes did not land.
func TestWriteFileAtomic_WriteAndCloseErrorsSurface(t *testing.T) {
	dir := t.TempDir()
	orig := createTemp
	t.Cleanup(func() { createTemp = orig })

	createTemp = func(string) (tempFile, error) {
		f, err := os.CreateTemp(dir, ".tmp-*")
		require.NoError(t, err)
		name := f.Name()
		_ = f.Close()
		return &failTemp{name: name, writeErr: assert.AnError}, nil
	}
	assert.ErrorIs(t, WriteFileAtomic(filepath.Join(dir, "k"), []byte("x")), assert.AnError, "write failure surfaces")

	createTemp = func(string) (tempFile, error) {
		f, err := os.CreateTemp(dir, ".tmp-*")
		require.NoError(t, err)
		name := f.Name()
		_ = f.Close()
		return &failTemp{name: name, closeErr: assert.AnError}, nil
	}
	assert.ErrorIs(t, WriteFileAtomic(filepath.Join(dir, "k"), []byte("x")), assert.AnError, "close failure surfaces")
}

func TestReadPub_ValidAbsentAndMalformed(t *testing.T) {
	dir := t.TempDir()
	pub, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)

	valid := filepath.Join(dir, "good.pub")
	require.NoError(t, WriteFileAtomic(valid, pub))
	assert.Equal(t, pub, ReadPub(valid), "a valid key round-trips")

	assert.Nil(t, ReadPub(filepath.Join(dir, "absent.pub")), "absent file → nil")

	bad := filepath.Join(dir, "bad.pub")
	require.NoError(t, WriteFileAtomic(bad, []byte("too short")))
	assert.Nil(t, ReadPub(bad), "wrong-size file → nil")
}
