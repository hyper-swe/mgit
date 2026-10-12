package images_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/sandboxd/basecache"
	"github.com/hyper-swe/mgit/internal/sandboxd/images"
)

func TestCachedPins_NamesEveryCacheLocatedEntryAndNoPathEntry(t *testing.T) {
	cache, err := basecache.New(t.TempDir())
	require.NoError(t, err)
	hostRoot, priv := hostRootWithTrust(t)
	a := publishBase(t, cache, map[string]string{"a": "1"})
	b := publishBase(t, cache, map[string]string{"b": "2"})
	byo := t.TempDir()
	byoEntry, err := images.BuildBaseEntry(byo)
	require.NoError(t, err)

	_, err = images.Register(hostRoot, "base", images.BuildCachedBaseEntry(a.Digest), priv)
	require.NoError(t, err)
	_, err = images.Register(hostRoot, "other", images.BuildCachedBaseEntry(b.Digest), priv)
	require.NoError(t, err)
	_, err = images.Register(hostRoot, "byo", byoEntry, priv)
	require.NoError(t, err)

	pins, err := images.CachedPins(hostRoot)
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{a.Digest: true, b.Digest: true}, pins,
		"a base pinned by path lives outside the cache and is no pin on a cache entry")
}

func TestCachedPins_NoLock_PinsNothing(t *testing.T) {
	pins, err := images.CachedPins(t.TempDir())
	require.NoError(t, err)
	assert.Empty(t, pins)
}
