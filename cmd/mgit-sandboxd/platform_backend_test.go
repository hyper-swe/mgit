package main

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestImageResolver_LazyOpen_FailsClearlyWithoutTrustRoot verifies the
// resolver does not panic and surfaces a clear error when the host
// image store cannot be opened (no trust root configured). The daemon
// must still start; only the launch path fails. Refs: FR-17.10, FR-17.17
func TestImageResolver_LazyOpen_FailsClearlyWithoutTrustRoot(t *testing.T) {
	clock := func() time.Time { return time.Unix(0, 0).UTC() }
	resolve := newImageResolver(t.TempDir(), clock) // empty host root: no trust root

	_, err := resolve("img@sha256:" + strings.Repeat("a", 64))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "image store", "the error must name the unavailable image store")
}

// TestImageResolver_OpensStoreOnce verifies the store is opened lazily
// and only once across calls (the open failure is cached, not retried
// per call). Refs: FR-17.10
func TestImageResolver_OpensStoreOnce(t *testing.T) {
	clock := func() time.Time { return time.Unix(0, 0).UTC() }
	resolve := newImageResolver(t.TempDir(), clock)

	_, err1 := resolve("img@sha256:" + strings.Repeat("a", 64))
	_, err2 := resolve("img@sha256:" + strings.Repeat("b", 64))
	require.Error(t, err1)
	require.Error(t, err2)
	assert.Equal(t, err1.Error(), err2.Error(), "the cached open failure is returned identically each call")
}
