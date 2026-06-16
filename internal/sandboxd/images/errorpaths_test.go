package images

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/sandboxd/hostkey"
)

// TestStore_Guards covers NewStore validation and fail-closed loading.
func TestStore_Guards(t *testing.T) {
	t.Run("empty_host_root", func(t *testing.T) {
		_, err := NewStore("", fixedClock())
		assert.Error(t, err)
	})
	t.Run("nil_clock", func(t *testing.T) {
		_, err := NewStore(t.TempDir(), nil)
		assert.Error(t, err)
	})
	t.Run("missing_trust_root_fails_closed", func(t *testing.T) {
		_, err := NewStore(t.TempDir(), fixedClock())
		assert.Error(t, err, "no trust root means nothing can be verified")
	})
	t.Run("malformed_trust_root_rejected", func(t *testing.T) {
		hostRoot := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(hostRoot, hostkey.TrustDirName), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(hostRoot, hostkey.TrustDirName, signingPub),
			[]byte("too-short"), 0o600))
		_, err := NewStore(hostRoot, fixedClock())
		assert.Error(t, err)
	})
}

// TestResolve_LockReadFailures covers the readLock error paths.
func TestResolve_LockReadFailures(t *testing.T) {
	fx := newFixture(t)

	t.Run("absent_lock", func(t *testing.T) {
		require.NoError(t, os.Remove(filepath.Join(fx.hostRoot, lockFileName)))
		_, err := fx.store.Resolve(fx.imageRef)
		assert.Error(t, err)
	})

	t.Run("malformed_lock", func(t *testing.T) {
		require.NoError(t, os.WriteFile(filepath.Join(fx.hostRoot, lockFileName),
			[]byte("{not json"), 0o600))
		_, err := fx.store.Resolve(fx.imageRef)
		assert.Error(t, err)
	})

	t.Run("missing_rootfs_file", func(t *testing.T) {
		fx := newFixture(t)
		require.NoError(t, os.Remove(fx.rootfsPath))
		_, err := fx.store.Resolve(fx.imageRef)
		assert.Error(t, err)
	})
}

// TestGenerateTrustRoot_Guards covers the constructor guards.
func TestGenerateTrustRoot_Guards(t *testing.T) {
	ctx := context.Background()
	_, err := GenerateTrustRoot(ctx, "", &recordingAudit{})
	assert.Error(t, err, "empty host root rejected")
	_, err = GenerateTrustRoot(ctx, t.TempDir(), nil)
	assert.Error(t, err, "nil auditor rejected")
}

// failAudit fails the trust-root record.
type failAudit struct{}

func (failAudit) RecordTrustRootChange(context.Context, string) error {
	return assert.AnError
}

// TestGenerateTrustRoot_AuditFailureSurfaces verifies an unrecordable
// generation surfaces the error.
func TestGenerateTrustRoot_AuditFailureSurfaces(t *testing.T) {
	_, err := GenerateTrustRoot(context.Background(), t.TempDir(), failAudit{})
	assert.Error(t, err)
}

// failSnapshotter fails clean-base snapshotting.
type failSnapshotter struct{}

func (failSnapshotter) SnapshotCleanBase(context.Context, string) ([]byte, error) {
	return nil, assert.AnError
}

// TestWarmPool_PrimeFailureSurfaces verifies a failed clean-base boot
// surfaces and stores nothing.
func TestWarmPool_PrimeFailureSurfaces(t *testing.T) {
	pool := NewWarmPool(failSnapshotter{}, fixedClock())
	ctx := context.Background()
	digest := "sha256:deadbeef"

	err := pool.Prime(ctx, digest)
	require.Error(t, err)
	_, err = pool.Acquire(ctx, digest)
	assert.ErrorIs(t, err, ErrNoWarmSnapshot, "a failed prime stores nothing")
}

// TestWarmPool_RePrimeZeroesPrevious verifies re-priming a digest
// zeroes the superseded snapshot's memory (SEC-08).
func TestWarmPool_RePrimeZeroesPrevious(t *testing.T) {
	snapshotter := &fakeSnapshotter{}
	pool := NewWarmPool(snapshotter, fixedClock())
	ctx := context.Background()
	digest := "sha256:" + "d"

	require.NoError(t, pool.Prime(ctx, digest))
	pool.mu.Lock()
	first := pool.snapshots[digest]
	firstBuf := first.Memory
	pool.mu.Unlock()

	require.NoError(t, pool.Prime(ctx, digest))
	for i, b := range firstBuf {
		require.Zero(t, b, "superseded snapshot memory must be zeroed, byte %d", i)
	}
}
