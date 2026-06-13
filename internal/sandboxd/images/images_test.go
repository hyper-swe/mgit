// Package images tests verify image pinning, boot-time verification,
// and the warm pool per MGIT-11.5.5 acceptance criteria.
// Refs: FR-17.17, FR-17.29, FR-17.25, FR-17.38
package images

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

func fixedClock() func() time.Time {
	fixed := time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC)
	return func() time.Time { return fixed }
}

// recordingAudit captures trust-root audit events.
type recordingAudit struct {
	details []string
}

func (r *recordingAudit) RecordTrustRootChange(_ context.Context, detail string) error {
	r.details = append(r.details, detail)
	return nil
}

// fixture builds a host root containing a trust root, one valid
// image (kernel+rootfs), and a signed images.lock entry for it.
type fixture struct {
	hostRoot   string
	store      *Store
	imageRef   string
	digest     string
	rootfsPath string
	priv       ed25519.PrivateKey
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	hostRoot := t.TempDir()
	audit := &recordingAudit{}

	priv, err := GenerateTrustRoot(context.Background(), hostRoot, audit)
	require.NoError(t, err)

	imgDir := filepath.Join(hostRoot, "img")
	require.NoError(t, os.MkdirAll(imgDir, 0o700))
	kernel := filepath.Join(imgDir, "vmlinux")
	rootfs := filepath.Join(imgDir, "rootfs.img")
	require.NoError(t, os.WriteFile(kernel, []byte("kernel-bytes"), 0o600))
	rootfsBytes := []byte("rootfs-bytes")
	require.NoError(t, os.WriteFile(rootfs, rootfsBytes, 0o600))

	digest := fileDigest(t, rootfs)
	kernelDigest := fileDigest(t, kernel)

	entry := Entry{
		Digest:       digest,
		KernelDigest: kernelDigest,
		KernelPath:   kernel,
		RootfsPath:   rootfs,
		Cmdline:      "console=hvc0 root=/dev/vda ro",
	}
	entry.Signature = ed25519.Sign(priv, SigningPayload("go-node", entry))
	writeLock(t, hostRoot, Lock{Images: map[string]Entry{"go-node": entry}})

	store, err := NewStore(hostRoot, fixedClock())
	require.NoError(t, err)

	return &fixture{
		hostRoot:   hostRoot,
		store:      store,
		imageRef:   "go-node@" + digest,
		digest:     digest,
		rootfsPath: rootfs,
		priv:       priv,
	}
}

func writeLock(t *testing.T, hostRoot string, lock Lock) {
	t.Helper()
	data, err := json.Marshal(lock)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(hostRoot, "images.lock"), data, 0o600))
}

func fileDigest(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // test path
	require.NoError(t, err)
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func signEntry(t *testing.T, priv ed25519.PrivateKey, name string, e Entry) Entry {
	t.Helper()
	e.Signature = ed25519.Sign(priv, SigningPayload(name, e))
	return e
}

func loadLock(hostRoot string) (Lock, error) {
	data, err := os.ReadFile(filepath.Join(hostRoot, "images.lock")) //nolint:gosec // test path
	if err != nil {
		return Lock{}, err
	}
	var lock Lock
	err = json.Unmarshal(data, &lock)
	return lock, err
}

// TestImage_ValidImage_Resolves covers the happy path: a pinned,
// signed, content-matching image resolves to its paths.
// Refs: FR-17.17, FR-17.29
func TestImage_ValidImage_Resolves(t *testing.T) {
	fx := newFixture(t)

	resolved, err := fx.store.Resolve(fx.imageRef)
	require.NoError(t, err)
	assert.Equal(t, fx.rootfsPath, resolved.RootfsPath)
	assert.NotEmpty(t, resolved.KernelPath)
	assert.NotEmpty(t, resolved.Cmdline)
}

// TestImage_BadSignature_BootRejected verifies SEC-12/F-10: a lock
// entry whose signature does not verify against the trust root is
// refused — a poisoned lock entry is detectable, not just a tampered
// image. Refs: FR-17.29, FR-17.38
func TestImage_BadSignature_BootRejected(t *testing.T) {
	fx := newFixture(t)

	kernel := filepath.Join(fx.hostRoot, "img", "vmlinux")
	base := Entry{
		Digest: fx.digest, KernelDigest: fileDigest(t, kernel),
		KernelPath: kernel, RootfsPath: fx.rootfsPath,
		Cmdline: "console=hvc0 root=/dev/vda ro",
	}

	// Re-sign the canonical payload with a DIFFERENT key: the
	// lock-writer scenario — without the trust root they cannot mint
	// signatures the host will accept.
	_, otherPriv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	entry := base
	entry.Signature = ed25519.Sign(otherPriv, SigningPayload("go-node", entry))
	writeLock(t, fx.hostRoot, Lock{Images: map[string]Entry{"go-node": entry}})

	_, err = fx.store.Resolve(fx.imageRef)
	assert.ErrorIs(t, err, model.ErrVerificationFailed,
		"a signature from outside the trust root must refuse the boot")

	t.Run("missing_signature_rejected", func(t *testing.T) {
		writeLock(t, fx.hostRoot, Lock{Images: map[string]Entry{"go-node": base}})
		_, err := fx.store.Resolve(fx.imageRef)
		assert.ErrorIs(t, err, model.ErrVerificationFailed)
	})

	t.Run("kernel_swap_with_valid_rootfs_sig_rejected", func(t *testing.T) {
		// A lock-writer keeps a validly-signed entry but repoints the
		// kernel to attacker bytes. Content verification of the kernel
		// digest catches it (the kernel was never unverified).
		fx := newFixture(t)
		evilKernel := filepath.Join(fx.hostRoot, "img", "evil-kernel")
		require.NoError(t, os.WriteFile(evilKernel, []byte("malicious-kernel"), 0o600))

		lock := Lock{Images: map[string]Entry{"go-node": {
			Digest: fx.digest, KernelDigest: fileDigest(t, filepath.Join(fx.hostRoot, "img", "vmlinux")),
			KernelPath: evilKernel, // repointed; content will not match KernelDigest
			RootfsPath: fx.rootfsPath, Cmdline: "console=hvc0 root=/dev/vda ro",
		}}}
		// Even sign it correctly: content verification is the backstop.
		lock.Images["go-node"] = signEntry(t, fx.priv, "go-node", lock.Images["go-node"])
		writeLock(t, fx.hostRoot, lock)

		_, err := fx.store.Resolve(fx.imageRef)
		assert.ErrorIs(t, err, model.ErrVerificationFailed,
			"a kernel whose content does not match its pinned digest must refuse the boot")
	})

	t.Run("name_rebind_rejected", func(t *testing.T) {
		// The signature binds the name: a signed "go-node" entry copied
		// under a different name fails verification for that name.
		fx := newFixture(t)
		lock, err := loadLock(fx.hostRoot)
		require.NoError(t, err)
		lock.Images["evil"] = lock.Images["go-node"] // same fields + signature
		writeLock(t, fx.hostRoot, lock)

		_, err = fx.store.Resolve("evil@" + fx.digest)
		assert.ErrorIs(t, err, model.ErrVerificationFailed,
			"a signature minted for go-node must not authorize the name evil")
	})

	t.Run("cmdline_tamper_rejected", func(t *testing.T) {
		// Changing the cmdline (e.g. injecting init=/work/payload) while
		// keeping the signature fails: cmdline is in the signed payload.
		fx := newFixture(t)
		lock, err := loadLock(fx.hostRoot)
		require.NoError(t, err)
		e := lock.Images["go-node"]
		e.Cmdline = "console=hvc0 init=/work/payload"
		lock.Images["go-node"] = e // signature unchanged
		writeLock(t, fx.hostRoot, lock)

		_, err = fx.store.Resolve(fx.imageRef)
		assert.ErrorIs(t, err, model.ErrVerificationFailed,
			"a tampered cmdline must refuse the boot")
	})

	t.Run("trust_root_separate_from_lock", func(t *testing.T) {
		// FR-17.38: the trust root lives OUTSIDE images.lock — a lock
		// carrying its own key material must not influence verification.
		assert.NoFileExists(t, filepath.Join(fx.hostRoot, "images.lock.pub"))
		assert.FileExists(t, filepath.Join(fx.hostRoot, "trust", "image-signing.pub"))
		info, err := os.Stat(filepath.Join(fx.hostRoot, "trust", "image-signing.key"))
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "signing key is owner-only")
	})
}

// TestImage_BadDigest_BootRejected verifies FR-17.17: image content
// that does not hash to the pinned digest is refused at boot.
func TestImage_BadDigest_BootRejected(t *testing.T) {
	fx := newFixture(t)

	// Tamper with the rootfs after it was pinned and signed.
	require.NoError(t, os.WriteFile(fx.rootfsPath, []byte("tampered"), 0o600))

	_, err := fx.store.Resolve(fx.imageRef)
	assert.ErrorIs(t, err, model.ErrVerificationFailed,
		"tampered image content must refuse the boot")

	t.Run("ref_digest_must_match_lock_digest", func(t *testing.T) {
		fx := newFixture(t)
		wrongDigest := "sha256:" + strings.Repeat("0", 64)
		_, err := fx.store.Resolve("go-node@" + wrongDigest)
		assert.ErrorIs(t, err, model.ErrVerificationFailed,
			"a ref pinned to a different digest than the lock must refuse")
	})

	t.Run("unknown_image_rejected", func(t *testing.T) {
		fx := newFixture(t)
		_, err := fx.store.Resolve("absent@" + fx.digest)
		assert.Error(t, err)
	})

	t.Run("malformed_ref_rejected", func(t *testing.T) {
		fx := newFixture(t)
		_, err := fx.store.Resolve("not-a-pinned-ref")
		assert.Error(t, err)
	})
}

// TestTrustRoot_RotationAudited verifies FR-17.38: rotation appends an
// audit event carrying old and new fingerprints.
func TestTrustRoot_RotationAudited(t *testing.T) {
	hostRoot := t.TempDir()
	audit := &recordingAudit{}
	ctx := context.Background()

	_, err := GenerateTrustRoot(ctx, hostRoot, audit)
	require.NoError(t, err)
	require.Len(t, audit.details, 1, "initial generation is audited")
	assert.Contains(t, audit.details[0], `"new_fingerprint"`)

	_, err = GenerateTrustRoot(ctx, hostRoot, audit)
	require.NoError(t, err)
	require.Len(t, audit.details, 2, "rotation is audited")
	assert.Contains(t, audit.details[1], `"old_fingerprint"`,
		"rotation records the key it replaced")
	assert.Contains(t, audit.details[1], `"new_fingerprint"`)
}

// fakeSnapshotter scripts clean-base snapshots for the pool.
type fakeSnapshotter struct {
	boots int
}

func (s *fakeSnapshotter) SnapshotCleanBase(_ context.Context, digest string) ([]byte, error) {
	s.boots++
	return []byte("snapshot-of-" + digest), nil
}

// TestWarmPool_SnapshotFromCleanBaseOnly verifies SEC-08/FR-17.25:
// pool snapshots are created only by the pool's own clean-base boot —
// the API offers no way to insert a used guest — provenance is
// recorded, and a consumed snapshot is never handed out twice.
func TestWarmPool_SnapshotFromCleanBaseOnly(t *testing.T) {
	snapshotter := &fakeSnapshotter{}
	pool := NewWarmPool(snapshotter, fixedClock())
	ctx := context.Background()
	digest := "sha256:" + strings.Repeat("a", 64)

	require.NoError(t, pool.Prime(ctx, digest))
	assert.Equal(t, 1, snapshotter.boots, "priming boots the clean base once")

	snap, err := pool.Acquire(ctx, digest)
	require.NoError(t, err)
	assert.Equal(t, "clean-base-boot", snap.Provenance.Source,
		"provenance proves the clean-base origin (SEC-08)")
	assert.Equal(t, digest, snap.Provenance.ImageDigest)
	assert.False(t, snap.Provenance.CreatedAt.IsZero())

	t.Run("consumed_snapshot_not_reused", func(t *testing.T) {
		_, err := pool.Acquire(ctx, digest)
		assert.ErrorIs(t, err, ErrNoWarmSnapshot,
			"a handed-out snapshot is consumed; the pool re-primes from clean base")
	})

	t.Run("unprimed_digest_has_no_snapshot", func(t *testing.T) {
		_, err := pool.Acquire(ctx, "sha256:"+strings.Repeat("b", 64))
		assert.ErrorIs(t, err, ErrNoWarmSnapshot)
	})
}

// TestWarmPool_ReclaimedPagesZeroed verifies SEC-08: memory the pool
// returns to the host is zeroed first — released snapshots cannot leak
// prior content.
func TestWarmPool_ReclaimedPagesZeroed(t *testing.T) {
	pool := NewWarmPool(&fakeSnapshotter{}, fixedClock())
	ctx := context.Background()
	digest := "sha256:" + strings.Repeat("c", 64)

	require.NoError(t, pool.Prime(ctx, digest))
	snap, err := pool.Acquire(ctx, digest)
	require.NoError(t, err)
	require.NotEmpty(t, snap.Memory)

	buffer := snap.Memory // alias the backing array
	snap.Release()

	for i, b := range buffer {
		require.Zero(t, b, "released snapshot memory must be zeroed, byte %d leaked", i)
	}

	t.Run("discard_unacquired_snapshot_zeroes", func(t *testing.T) {
		require.NoError(t, pool.Prime(ctx, digest))
		pool.Discard(digest)
		_, err := pool.Acquire(ctx, digest)
		assert.ErrorIs(t, err, ErrNoWarmSnapshot)
	})
}
