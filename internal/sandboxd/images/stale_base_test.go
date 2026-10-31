package images_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/basecache"
	"github.com/hyper-swe/mgit/internal/sandboxd/images"
)

func TestStaleBase_RefusalNamesBothPinsAndExactReplacement(t *testing.T) {
	cache, err := basecache.New(t.TempDir())
	require.NoError(t, err)
	host, key := hostRootWithTrust(t)
	old := publishBase(t, cache, map[string]string{"bin/sh": "original"})
	oldRef, err := images.Register(host, "base", images.BuildCachedBaseEntry(old.Digest), key)
	require.NoError(t, err)
	current := publishBase(t, cache, map[string]string{"bin/sh": "recomposed"})
	currentRef, err := images.Register(host, "base", images.BuildCachedBaseEntry(current.Digest), key)
	require.NoError(t, err)

	_, err = storeFor(t, host, cache).Resolve(oldRef)
	require.ErrorIs(t, err, model.ErrVerificationFailed)
	assert.Contains(t, err.Error(), old.Digest)
	assert.Contains(t, err.Error(), current.Digest)
	assert.Contains(t, err.Error(), "--image "+currentRef, "the remedy must name the exact current pin")
	assert.Contains(t, strings.ToLower(err.Error()), "remove this sandbox and launch it again")
	_, err = storeFor(t, host, cache).Resolve(currentRef)
	require.NoError(t, err, "the current signed pin still resolves")
	assert.DirExists(t, old.Path, "the old cache entry is retained, not silently trusted or deleted")
}
