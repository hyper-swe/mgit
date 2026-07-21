package imageinstall

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/sandboxd/images"
)

type noopAudit struct{}

func (noopAudit) RecordTrustRootChange(context.Context, string) error { return nil }

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(h[:])
}

// bundle writes a fixture image bundle (manifest.json + kernel + rootfs) for
// one platform into dir, returning the manifest it wrote.
func bundle(t *testing.T, dir, platform string, kernel, rootfs []byte) Manifest {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "vmlinux"), kernel, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "rootfs.ext4"), rootfs, 0o600))
	m := Manifest{
		Schema: 1,
		Images: map[string]PlatformImage{
			platform: {
				Kernel: "vmlinux", KernelSHA256: sha256Hex(kernel),
				Rootfs: "rootfs.ext4", RootfsSHA256: sha256Hex(rootfs),
				Cmdline: "console=ttyS0 root=/dev/vda ro init=/sbin/mgit-guest",
			},
		},
	}
	data, err := json.Marshal(m)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "manifest.json"), data, 0o600))
	return m
}

func installer(hostRoot string) *Installer {
	return &Installer{HostRoot: hostRoot, Platform: "linux/amd64", Audit: noopAudit{}}
}

// TestInstall_RegistersResolvableImage is the MGIT-61.1 core: install from a
// pinned bundle yields a registered, digest-pinned image that Store.Resolve
// admits — with the trust root auto-generated and the artifacts placed at
// stable paths under the host root.
func TestInstall_RegistersResolvableImage(t *testing.T) {
	src := t.TempDir()
	bundle(t, src, "linux/amd64", []byte("fake-kernel-bytes"), []byte("fake-rootfs-bytes"))
	hostRoot := t.TempDir()

	res, err := installer(hostRoot).Install(context.Background(), src, "base")
	require.NoError(t, err)
	assert.Contains(t, res.Ref, "base@sha256:")
	assert.FileExists(t, res.KernelPath)
	assert.FileExists(t, res.RootfsPath)
	assert.Equal(t, filepath.Join(hostRoot, "images", "base"), filepath.Dir(res.KernelPath),
		"artifacts live at a stable path under the host root (images.lock stores absolute paths)")

	// The registered image resolves (signature + digest verify) to the
	// installed artifacts — proving the install produced a bootable entry.
	store, err := images.NewStore(hostRoot, func() time.Time { return time.Now().UTC() })
	require.NoError(t, err)
	resolved, err := store.Resolve(res.Ref)
	require.NoError(t, err)
	assert.Equal(t, res.KernelPath, resolved.KernelPath)
	assert.Equal(t, res.RootfsPath, resolved.RootfsPath)
}

// TestInstall_SHA256Mismatch_FailsClosed: a kernel whose bytes do not match
// the manifest digest is refused, nothing is registered, and the bad file is
// removed. Refs: MGIT-61.1
func TestInstall_SHA256Mismatch_FailsClosed(t *testing.T) {
	src := t.TempDir()
	bundle(t, src, "linux/amd64", []byte("real-kernel"), []byte("rootfs"))
	// Corrupt the kernel AFTER the manifest pinned the original's digest.
	require.NoError(t, os.WriteFile(filepath.Join(src, "vmlinux"), []byte("tampered-kernel"), 0o600))
	hostRoot := t.TempDir()

	_, err := installer(hostRoot).Install(context.Background(), src, "base")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sha256 mismatch")
	// Nothing registered.
	_, statErr := os.Stat(filepath.Join(hostRoot, "images.lock"))
	assert.True(t, os.IsNotExist(statErr), "a mismatch must not register an image")
}

// TestInstall_UnknownPlatform_Errors: a manifest without the host platform
// fails with a clear, listing error. Refs: MGIT-61.1
func TestInstall_UnknownPlatform_Errors(t *testing.T) {
	src := t.TempDir()
	bundle(t, src, "linux/amd64", []byte("k"), []byte("r"))
	hostRoot := t.TempDir()

	in := installer(hostRoot)
	in.Platform = "darwin/arm64" // not in the bundle
	_, err := in.Install(context.Background(), src, "base")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no image for darwin/arm64")
	assert.Contains(t, err.Error(), "linux/amd64", "the error lists what IS available")
}

// TestInstall_Idempotent: re-installing the same bundle succeeds and yields
// the same digest-pinned ref, reusing the existing trust root. Refs: MGIT-61.1
func TestInstall_Idempotent(t *testing.T) {
	src := t.TempDir()
	bundle(t, src, "linux/amd64", []byte("k"), []byte("r"))
	hostRoot := t.TempDir()
	in := installer(hostRoot)

	first, err := in.Install(context.Background(), src, "base")
	require.NoError(t, err)
	// Capture the trust-root key to prove it is NOT rotated on re-install.
	keyBefore := readTrustKey(t, hostRoot)

	second, err := in.Install(context.Background(), src, "base")
	require.NoError(t, err)
	assert.Equal(t, first.Ref, second.Ref, "same content → same digest-pinned ref")
	assert.Equal(t, keyBefore, readTrustKey(t, hostRoot), "re-install must not rotate the trust root")
}

// TestInstall_ReusesExistingTrustRoot: when a trust root already exists (from
// a prior `image init`), install signs with it rather than generating a new
// one. Refs: MGIT-61.1
func TestInstall_ReusesExistingTrustRoot(t *testing.T) {
	src := t.TempDir()
	bundle(t, src, "linux/amd64", []byte("k"), []byte("r"))
	hostRoot := t.TempDir()

	_, err := images.GenerateTrustRoot(context.Background(), hostRoot, noopAudit{})
	require.NoError(t, err)
	key := readTrustKey(t, hostRoot)

	_, err = installer(hostRoot).Install(context.Background(), src, "base")
	require.NoError(t, err)
	assert.Equal(t, key, readTrustKey(t, hostRoot), "install reuses the existing trust root")
}

func readTrustKey(t *testing.T, hostRoot string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(hostRoot, "trust", "image-signing.key")) //nolint:gosec // test path
	require.NoError(t, err)
	return b
}
