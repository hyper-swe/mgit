package images

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// registerable writes a kernel + rootfs under a fresh host root with a
// trust root, and returns (hostRoot, kernelPath, rootfsPath, priv).
func registerable(t *testing.T) (hostRoot, kernel, rootfs string) {
	t.Helper()
	hostRoot = t.TempDir()
	_, err := GenerateTrustRoot(context.Background(), hostRoot, &recordingAudit{})
	require.NoError(t, err)
	dir := filepath.Join(hostRoot, "img")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	kernel = filepath.Join(dir, "vmlinux")
	rootfs = filepath.Join(dir, "rootfs.img")
	require.NoError(t, os.WriteFile(kernel, []byte("kernel-bytes"), 0o600))
	require.NoError(t, os.WriteFile(rootfs, []byte("rootfs-bytes"), 0o600))
	return hostRoot, kernel, rootfs
}

// TestRegister_RoundTrip is the core proof: an image built, signed, and
// registered with the host signer is then resolved+verified by the Store
// exactly as a boot would. Refs: FR-17.17, FR-17.29
func TestRegister_RoundTrip(t *testing.T) {
	hostRoot, kernel, rootfs := registerable(t)
	priv, err := LoadSigningKey(hostRoot)
	require.NoError(t, err)

	entry, err := BuildEntry(kernel, rootfs, "console=ttyS0 init=/sbin/mgit-guest")
	require.NoError(t, err)
	ref, err := Register(hostRoot, "mgit-guest", entry, priv)
	require.NoError(t, err)
	assert.Equal(t, "mgit-guest@"+entry.Digest, ref)

	store, err := NewStore(hostRoot, fixedClock())
	require.NoError(t, err)
	resolved, err := store.Resolve(ref)
	require.NoError(t, err)
	assert.Equal(t, rootfs, resolved.RootfsPath)
	assert.Equal(t, kernel, resolved.KernelPath)
	assert.Equal(t, "console=ttyS0 init=/sbin/mgit-guest", resolved.Cmdline)
}

// TestLoadSigningKey_RoundTrip verifies the loaded key is the one
// GenerateTrustRoot produced (it signs a payload the trust pub verifies).
func TestLoadSigningKey_RoundTrip(t *testing.T) {
	hostRoot := t.TempDir()
	gen, err := GenerateTrustRoot(context.Background(), hostRoot, &recordingAudit{})
	require.NoError(t, err)
	loaded, err := LoadSigningKey(hostRoot)
	require.NoError(t, err)
	assert.Equal(t, []byte(gen), []byte(loaded))
}

// TestLoadSigningKey_Missing fails closed without a trust root.
func TestLoadSigningKey_Missing(t *testing.T) {
	_, err := LoadSigningKey(t.TempDir())
	assert.Error(t, err)
}

// TestRegister_PreservesOtherEntries verifies registering a second image
// keeps the first resolvable (images.lock is updated, not replaced).
func TestRegister_PreservesOtherEntries(t *testing.T) {
	hostRoot, kernel, rootfs := registerable(t)
	priv, err := LoadSigningKey(hostRoot)
	require.NoError(t, err)
	entry, err := BuildEntry(kernel, rootfs, "cmdline-a")
	require.NoError(t, err)

	refA, err := Register(hostRoot, "image-a", entry, priv)
	require.NoError(t, err)
	refB, err := Register(hostRoot, "image-b", entry, priv)
	require.NoError(t, err)

	store, err := NewStore(hostRoot, fixedClock())
	require.NoError(t, err)
	_, err = store.Resolve(refA)
	assert.NoError(t, err, "first image still resolves after a second is registered")
	_, err = store.Resolve(refB)
	assert.NoError(t, err)
}

// TestComputeDigest matches the standard library over the same bytes.
func TestComputeDigest(t *testing.T) {
	f := filepath.Join(t.TempDir(), "x")
	require.NoError(t, os.WriteFile(f, []byte("payload"), 0o600))
	sum := sha256.Sum256([]byte("payload"))
	got, err := ComputeDigest(f)
	require.NoError(t, err)
	assert.Equal(t, "sha256:"+hex.EncodeToString(sum[:]), got)
}

// TestBuildEntry_MissingFile fails closed on an absent kernel or rootfs.
func TestBuildEntry_MissingFile(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	require.NoError(t, os.WriteFile(real, []byte("x"), 0o600))
	absent := filepath.Join(dir, "absent")
	_, err := BuildEntry(absent, real, "c")
	assert.Error(t, err)
	_, err = BuildEntry(real, absent, "c")
	assert.Error(t, err)
}

// TestRegister_Guards covers the input guards.
func TestRegister_Guards(t *testing.T) {
	hostRoot, kernel, rootfs := registerable(t)
	priv, err := LoadSigningKey(hostRoot)
	require.NoError(t, err)
	entry, err := BuildEntry(kernel, rootfs, "c")
	require.NoError(t, err)
	_, err = Register("", "n", entry, priv)
	assert.Error(t, err)
	_, err = Register(hostRoot, "", entry, priv)
	assert.Error(t, err)
}

// TestLoadSigningKey_Malformed fails closed on a wrong-length key file.
func TestLoadSigningKey_Malformed(t *testing.T) {
	hostRoot := t.TempDir()
	_, err := GenerateTrustRoot(context.Background(), hostRoot, &recordingAudit{})
	require.NoError(t, err)
	keyPath := filepath.Join(hostRoot, "trust", signingKey)
	require.NoError(t, os.WriteFile(keyPath, []byte("too-short"), 0o600))
	_, err = LoadSigningKey(hostRoot)
	assert.Error(t, err)
}

// TestRegister_CorruptLockSurfaces verifies a malformed images.lock fails
// the registration rather than silently overwriting it.
func TestRegister_CorruptLockSurfaces(t *testing.T) {
	hostRoot, kernel, rootfs := registerable(t)
	priv, err := LoadSigningKey(hostRoot)
	require.NoError(t, err)
	entry, err := BuildEntry(kernel, rootfs, "c")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(hostRoot, lockFileName), []byte("{not json"), 0o600))
	_, err = Register(hostRoot, "img", entry, priv)
	assert.Error(t, err)
}

// TestRegister_TamperedRootfsRejectedAtResolve verifies that if the rootfs
// content changes after registration, Resolve fails closed (the signature
// covers the pinned digest, and content is re-hashed at boot).
func TestRegister_TamperedRootfsRejectedAtResolve(t *testing.T) {
	hostRoot, kernel, rootfs := registerable(t)
	priv, err := LoadSigningKey(hostRoot)
	require.NoError(t, err)
	entry, err := BuildEntry(kernel, rootfs, "c")
	require.NoError(t, err)
	ref, err := Register(hostRoot, "img", entry, priv)
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(rootfs, []byte("tampered-after-register"), 0o600))
	store, err := NewStore(hostRoot, fixedClock())
	require.NoError(t, err)
	_, err = store.Resolve(ref)
	assert.Error(t, err, "a rootfs changed after registration must not resolve")
}
