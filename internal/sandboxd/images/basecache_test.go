package images_test

import (
	"context"
	"crypto/ed25519"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/basecache"
	"github.com/hyper-swe/mgit/internal/sandboxd/images"
)

// publishBase composes a tree with the given files into the shared cache and
// returns its published entry.
func publishBase(t *testing.T, cache *basecache.Cache, files map[string]string) basecache.Entry {
	t.Helper()
	staging, err := cache.Stage()
	require.NoError(t, err)
	for name, content := range files {
		path := filepath.Join(staging, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	}
	entry, err := cache.Commit(staging, images.TreeDigest)
	require.NoError(t, err)
	return entry
}

// hostRootWithTrust makes a host config root with a signing key, standing in
// for one repository's .mgit/sandbox.
func hostRootWithTrust(t *testing.T) (string, ed25519.PrivateKey) {
	t.Helper()
	root := t.TempDir()
	priv, err := images.GenerateTrustRoot(t.Context(), root, noopAuditor{})
	require.NoError(t, err)
	return root, priv
}

// noopAuditor satisfies images.TrustRootAuditor; trust-root auditing is
// covered by the trust-root tests, not by these.
type noopAuditor struct{}

func (noopAuditor) RecordTrustRootChange(_ context.Context, _ string) error { return nil }

func storeFor(t *testing.T, hostRoot string, cache *basecache.Cache) *images.Store {
	t.Helper()
	store, err := images.NewStoreWithBaseCache(hostRoot, time.Now, cache)
	require.NoError(t, err)
	return store
}

// THE REGRESSION THIS TICKET EXISTS FOR. Two worktrees share one machine and
// one base cache. One of them recomposes its base; the other's pinned digest
// must still resolve, because the recompose published a NEW entry rather than
// rewriting the old one. Before MGIT-147 this failed closed with
// "hashes to sha256:3bb5c3c2..., pinned sha256:974b1b1e..." — twice, live.
// Refs: MGIT-147
func TestResolve_RecomposingInOneWorktree_DoesNotInvalidateAnothersPin(t *testing.T) {
	cacheRoot := t.TempDir()
	cache, err := basecache.New(cacheRoot)
	require.NoError(t, err)

	// Worktree A composes a base and pins it.
	original := publishBase(t, cache, map[string]string{"bin/sh": "v1", "etc/os-release": "ID=debian"})
	hostA, privA := hostRootWithTrust(t)
	refA, err := images.Register(hostA, "base", images.BuildCachedBaseEntry(original.Digest), privA)
	require.NoError(t, err)

	// Worktree B recomposes — different bytes, therefore a different digest.
	recomposed := publishBase(t, cache, map[string]string{"bin/sh": "v2", "etc/os-release": "ID=debian"})
	hostB, privB := hostRootWithTrust(t)
	refB, err := images.Register(hostB, "base", images.BuildCachedBaseEntry(recomposed.Digest), privB)
	require.NoError(t, err)
	require.NotEqual(t, refA, refB, "a recompose with different bytes must produce a different pin")

	// A's pin still resolves, and still points at A's bytes.
	resolvedA, err := storeFor(t, hostA, cache).Resolve(refA)
	require.NoError(t, err, "worktree B's recompose invalidated worktree A's pin")
	content, err := os.ReadFile(filepath.Join(resolvedA.RootfsPath, "bin", "sh")) //nolint:gosec // test-owned path
	require.NoError(t, err)
	assert.Equal(t, "v1", string(content), "worktree A booted worktree B's base")

	// And B's resolves to B's.
	resolvedB, err := storeFor(t, hostB, cache).Resolve(refB)
	require.NoError(t, err)
	assert.NotEqual(t, resolvedA.RootfsPath, resolvedB.RootfsPath)
}

// The pinned digest is the path, so a lock entry carries no base bytes and no
// base path at all. Refs: MGIT-147
func TestBuildCachedBaseEntry_RecordsTheDigestAndNoPath(t *testing.T) {
	entry := images.BuildCachedBaseEntry("sha256:" + strings.Repeat("a", 64))
	assert.Equal(t, "sha256:"+strings.Repeat("a", 64), entry.Digest)
	assert.Empty(t, entry.RootfsPath, "a cached base is located by its digest, never by a stored path")
	assert.Empty(t, entry.KernelPath)
	assert.Empty(t, entry.KernelDigest)
}

// Verification did not become unnecessary — it became cheaper. Corrupt the
// cached bytes and the resolve must still fail closed. Refs: MGIT-147, FR-17.17
func TestResolve_CachedBaseWhoseBytesDoNotMatchItsName_FailsClosed(t *testing.T) {
	cache, err := basecache.New(t.TempDir())
	require.NoError(t, err)
	entry := publishBase(t, cache, map[string]string{"bin/sh": "v1"})
	hostRoot, priv := hostRootWithTrust(t)
	ref, err := images.Register(hostRoot, "base", images.BuildCachedBaseEntry(entry.Digest), priv)
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(filepath.Join(entry.Path, "bin", "sh"), []byte("tampered"), 0o600))

	_, err = storeFor(t, hostRoot, cache).Resolve(ref)
	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrVerificationFailed)
}

// A pin whose entry is not in this machine's cache must say so in terms that
// name the remedy — not fall through to a bare "no such file or directory".
// Refs: MGIT-147
func TestResolve_CachedBaseMissingFromTheCache_SaysHowToGetItBack(t *testing.T) {
	cache, err := basecache.New(t.TempDir())
	require.NoError(t, err)
	hostRoot, priv := hostRootWithTrust(t)

	entry := images.BuildCachedBaseEntry("sha256:" + strings.Repeat("b", 64))
	entry.Source = "registry-1.docker.io/library/golang:1.26-bookworm@sha256:" + strings.Repeat("c", 64)
	ref, err := images.Register(hostRoot, "base", entry, priv)
	require.NoError(t, err)

	_, err = storeFor(t, hostRoot, cache).Resolve(ref)
	require.Error(t, err)
	assert.ErrorIs(t, err, images.ErrBaseNotCached)
	msg := err.Error()
	assert.Contains(t, msg, "golang:1.26-bookworm", "the message must name the provenance it was composed from")
	assert.Contains(t, msg, "mgit sandbox base from", "the message must name the command that re-composes it")
	assert.Contains(t, msg, cache.Root(), "the message must name the cache that was searched")
}

func TestResolve_CachedBaseWithNoCacheConfigured_FailsClosedNotOpen(t *testing.T) {
	hostRoot, priv := hostRootWithTrust(t)
	ref, err := images.Register(hostRoot, "base",
		images.BuildCachedBaseEntry("sha256:"+strings.Repeat("d", 64)), priv)
	require.NoError(t, err)

	store, err := images.NewStoreWithBaseCache(hostRoot, time.Now, nil)
	require.NoError(t, err, "a missing cache must not stop the daemon starting")
	_, err = store.Resolve(ref)
	require.Error(t, err, "with no cache, a cached base must not resolve to anything")
	assert.ErrorIs(t, err, images.ErrBaseNotCached)
}

// Migration: an older mgit's lock points at an in-tree base. Once the bytes
// are adopted into the cache, the lock entry is repointed to the cache WITHOUT
// re-signing — paths are deliberately outside the signing payload — and the
// same pinned digest keeps resolving. Refs: MGIT-147
func TestRepointToCache_MovesTheLockOffTheInTreePath_KeepingThePinValid(t *testing.T) {
	cache, err := basecache.New(t.TempDir())
	require.NoError(t, err)
	hostRoot, priv := hostRootWithTrust(t)

	legacy := filepath.Join(hostRoot, "base")
	require.NoError(t, os.MkdirAll(filepath.Join(legacy, "bin"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(legacy, "bin", "sh"), []byte("v1"), 0o600))

	legacyEntry, err := images.BuildBaseEntry(legacy)
	require.NoError(t, err)
	ref, err := images.Register(hostRoot, "base", legacyEntry, priv)
	require.NoError(t, err)

	adopted, err := cache.Adopt(legacy, images.TreeDigest)
	require.NoError(t, err)
	require.Equal(t, legacyEntry.Digest, adopted.Digest, "adoption must preserve the digest")

	repointed, err := images.RepointToCache(hostRoot, legacy)
	require.NoError(t, err)
	assert.Equal(t, []string{"base"}, repointed)

	resolved, err := storeFor(t, hostRoot, cache).Resolve(ref)
	require.NoError(t, err, "the pin stopped resolving across the migration")
	assert.Equal(t, adopted.Path, resolved.RootfsPath)

	// The repo holds no base bytes any more.
	assert.NoDirExists(t, legacy)
}

func TestRepointToCache_LeavesUnrelatedEntriesAlone(t *testing.T) {
	hostRoot, priv := hostRootWithTrust(t)
	other := filepath.Join(t.TempDir(), "byo-base")
	require.NoError(t, os.MkdirAll(filepath.Join(other, "bin"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(other, "bin", "sh"), []byte("v1"), 0o600))
	entry, err := images.BuildBaseEntry(other)
	require.NoError(t, err)
	_, err = images.Register(hostRoot, "byo", entry, priv)
	require.NoError(t, err)

	repointed, err := images.RepointToCache(hostRoot, filepath.Join(hostRoot, "base"))
	require.NoError(t, err)
	assert.Empty(t, repointed)

	got, err := images.LookupEntry(hostRoot, "byo")
	require.NoError(t, err)
	assert.Equal(t, other, got.RootfsPath, "a bring-your-own base must keep pointing at the user's tree")
}

func TestLookupEntry_UnknownName_ReportsNoSuchImage(t *testing.T) {
	hostRoot, _ := hostRootWithTrust(t)
	_, err := images.LookupEntry(hostRoot, "base")
	assert.ErrorIs(t, err, images.ErrNoSuchImage)
}

func TestLegacyInTreeBase_ReportsAnInTreeBaseAndNothingElse(t *testing.T) {
	hostRoot := t.TempDir()
	assert.Empty(t, images.LegacyInTreeBase(hostRoot), "a clean repo has no in-tree base")

	legacy := filepath.Join(hostRoot, images.InTreeBaseDir)
	require.NoError(t, os.MkdirAll(legacy, 0o750))
	assert.Equal(t, legacy, images.LegacyInTreeBase(hostRoot))

	// A FILE of that name is not a base and must not be reported as one.
	other := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(other, images.InTreeBaseDir), []byte("x"), 0o600))
	assert.Empty(t, images.LegacyInTreeBase(other))
}
