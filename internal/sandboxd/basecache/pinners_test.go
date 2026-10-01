package basecache_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/sandboxd/basecache"
	"github.com/hyper-swe/mgit/internal/sandboxd/images"
)

// publish composes a one-file tree into the cache and returns its entry.
func publish(t *testing.T, cache *basecache.Cache, content string) basecache.Entry {
	t.Helper()
	entry, err := cache.Commit(treeWith(t, cache, map[string]string{"etc/os-release": content}), images.TreeDigest)
	require.NoError(t, err)
	return entry
}

func TestRecordPinner_SameRootTwice_RecordsOnce(t *testing.T) {
	cache := newCache(t)
	entry := publish(t, cache, "a")
	root := t.TempDir()

	require.NoError(t, cache.RecordPinner(entry.Digest, root))
	require.NoError(t, cache.RecordPinner(entry.Digest, root))

	pinners, recorded, err := cache.Pinners(entry.Digest)
	require.NoError(t, err)
	assert.True(t, recorded)
	require.Len(t, pinners, 1, "recording is idempotent per root")
	want, err := filepath.EvalSymlinks(root)
	require.NoError(t, err)
	assert.Equal(t, want, pinners[0], "the root is recorded by its resolved path")
}

func TestRecordPinner_TwoRoots_RecordsBoth(t *testing.T) {
	cache := newCache(t)
	entry := publish(t, cache, "a")
	a, b := t.TempDir(), t.TempDir()

	require.NoError(t, cache.RecordPinner(entry.Digest, a))
	require.NoError(t, cache.RecordPinner(entry.Digest, b))

	pinners, _, err := cache.Pinners(entry.Digest)
	require.NoError(t, err)
	assert.Len(t, pinners, 2)
}

func TestRecordPinner_ThroughASymlink_IsTheSameRoot(t *testing.T) {
	cache := newCache(t)
	entry := publish(t, cache, "a")
	root := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	require.NoError(t, os.Symlink(root, link))

	require.NoError(t, cache.RecordPinner(entry.Digest, root))
	require.NoError(t, cache.RecordPinner(entry.Digest, link))

	pinners, _, err := cache.Pinners(entry.Digest)
	require.NoError(t, err)
	assert.Len(t, pinners, 1, "a symlink to a recorded root names the same repository")
}

func TestPinners_EntryWithNoRecord_ReportsUnrecorded(t *testing.T) {
	cache := newCache(t)
	entry := publish(t, cache, "a")

	pinners, recorded, err := cache.Pinners(entry.Digest)
	require.NoError(t, err)
	assert.False(t, recorded, "an entry composed before back-references has no record")
	assert.Empty(t, pinners)
}

func TestRecordPinner_MalformedDigest_Refused(t *testing.T) {
	cache := newCache(t)
	err := cache.RecordPinner("sha256:../../etc", t.TempDir())
	require.Error(t, err)
}

func TestRecordPinner_KeepsBackRefsOutOfTheEntry(t *testing.T) {
	cache := newCache(t)
	entry := publish(t, cache, "a")
	require.NoError(t, cache.RecordPinner(entry.Digest, t.TempDir()))

	digest, err := images.TreeDigest(entry.Path)
	require.NoError(t, err)
	assert.Equal(t, entry.Digest, digest, "a back-reference never writes into a published entry")
}

func TestEntries_ListsPublishedEntriesOnly(t *testing.T) {
	cache := newCache(t)
	a := publish(t, cache, "a")
	b := publish(t, cache, "b")
	_, err := cache.Stage() // staging debris is not an entry
	require.NoError(t, err)

	listed, err := cache.Entries()
	require.NoError(t, err)
	var digests []string
	for _, e := range listed {
		digests = append(digests, e.Digest)
		assert.Equal(t, filepath.Join(cache.Root(), "sha256", strings.TrimPrefix(e.Digest, "sha256:")), e.Path)
	}
	assert.ElementsMatch(t, []string{a.Digest, b.Digest}, digests)
}

func TestEntries_EmptyCache_ListsNothing(t *testing.T) {
	listed, err := newCache(t).Entries()
	require.NoError(t, err)
	assert.Empty(t, listed)
}

func TestRemove_PublishedEntry_IsGoneAndOthersRemain(t *testing.T) {
	cache := newCache(t)
	a := publish(t, cache, "a")
	b := publish(t, cache, "b")

	require.NoError(t, cache.Remove(a.Digest))

	assert.False(t, cache.Has(a.Digest))
	assert.True(t, cache.Has(b.Digest))
	staged, err := os.ReadDir(filepath.Join(cache.Root(), "staging"))
	require.NoError(t, err)
	assert.Empty(t, staged, "a removal leaves no debris behind")
}

func TestRemove_AbsentEntry_ReturnsError(t *testing.T) {
	cache := newCache(t)
	a := publish(t, cache, "a")
	require.NoError(t, cache.Remove(a.Digest))
	require.Error(t, cache.Remove(a.Digest))
}

func TestTreeBytes_SumsRegularFiles(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"a": "12345", "sub/b": "678"})
	require.NoError(t, os.Symlink("a", filepath.Join(dir, "link")))

	n, err := basecache.TreeBytes(dir)
	require.NoError(t, err)
	assert.Equal(t, int64(8), n, "regular files only; a symlink is not followed or counted")
}
