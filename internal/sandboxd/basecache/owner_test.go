package basecache_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/sandboxd/basecache"
)

// IDENTITY, NOT SPELLING. A path comparison refused the entry's own path and
// a symlink to it, and accepted three other spellings of the very same
// directory: a case variant (APFS is case-insensitive), the firmlink path
// under /System/Volumes/Data, and any path while the process's own cache
// root pointed elsewhere. Each let one repository rewrite another's pinned
// base. The cache root now carries a marker, and Owner walks the given
// directory's ancestors looking for it. Refs: MGIT-226
func TestOwner_FindsTheCacheWhateverTheSpelling(t *testing.T) {
	cache := newCache(t)
	staged, err := cache.Stage()
	require.NoError(t, err)
	root := cache.Root()
	require.FileExists(t, filepath.Join(root, basecache.RootMarker), "staging a compose marks the root")
	entry := filepath.Join(root, "sha256", strings.Repeat("a", 64))
	require.NoError(t, os.MkdirAll(entry, 0o750))
	link := filepath.Join(t.TempDir(), "looks-like-mine")
	require.NoError(t, os.Symlink(entry, link))

	spellings := map[string]string{"its path": entry, "a symlink to it": link, "the root itself": root, "a staging tree": staged}
	if v := caseVariant(entry); v != "" {
		spellings["a case variant"] = v
	}
	if fl := firmlinkSpelling(entry); fl != "" {
		spellings["the firmlink path"] = fl
	}
	for name, dir := range spellings {
		got, err := basecache.Owner(dir, nil) // no known roots: the marker alone must decide
		require.NoError(t, err, name)
		assert.NotEmpty(t, got, "%s (%s) must be recognized as inside the cache", name, dir)
	}

	sibling := root + "-copy"
	require.NoError(t, os.MkdirAll(filepath.Join(sibling, "sha256"), 0o750))
	got, err := basecache.Owner(filepath.Join(sibling, "sha256"), nil)
	require.NoError(t, err)
	assert.Empty(t, got, "a sibling that only shares a prefix with the cache is not the cache")
}

// A CACHE WRITTEN BEFORE THE MARKER EXISTED carries none, and the machine-wide
// cache on every host that upgrades to this build is one. A root the caller
// names is still recognized, by identity rather than by spelling.
// Refs: MGIT-226
func TestOwner_AKnownRootWithoutAMarker_IsFoundByIdentity(t *testing.T) {
	root := t.TempDir()
	// Not entry-shaped, so neither the marker nor the layout decides: only
	// the root's identity can.
	entry := filepath.Join(root, "staging", "compose-1")
	require.NoError(t, os.MkdirAll(entry, 0o750))

	got, err := basecache.Owner(entry, nil)
	require.NoError(t, err)
	assert.Empty(t, got, "no marker and no known root: nothing says this is a cache")

	got, err = basecache.Owner(entry, []string{root})
	require.NoError(t, err)
	assert.NotEmpty(t, got, "a known root is found by identity")
	if v := caseVariant(entry); v != "" {
		got, err = basecache.Owner(v, []string{root})
		require.NoError(t, err)
		assert.NotEmpty(t, got, "and a case variant of it is the same directory")
	}
}

// Adopting a tree also publishes into the cache, so it marks the root too.
func TestAdopt_MarksTheCacheRoot(t *testing.T) {
	cache := newCache(t)
	dir := filepath.Join(t.TempDir(), "in-tree-base")
	writeTree(t, dir, map[string]string{"bin/sh": "#!/bin/sh"})
	_, err := cache.Adopt(dir, fixedDigester("sha256:"+strings.Repeat("c", 64)))
	require.NoError(t, err)
	assert.FileExists(t, filepath.Join(cache.Root(), basecache.RootMarker))
}

// The machine-wide location is where every repository's cache lives unless a
// process says otherwise, so it is checked even when this process does.
func TestSystemRoot_IgnoresTheOverride(t *testing.T) {
	t.Setenv(basecache.EnvRoot, t.TempDir())
	sys, err := basecache.SystemRoot()
	require.NoError(t, err)
	osCache, err := os.UserCacheDir()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(osCache, "mgit", "bases"), sys)
}

// caseVariant upper-cases the path's last component, and returns it only when
// this filesystem says it names the same directory.
func caseVariant(p string) string {
	v := filepath.Join(filepath.Dir(p), strings.ToUpper(filepath.Base(p)))
	a, errA := os.Stat(p)
	b, errB := os.Stat(v)
	if v == p || errA != nil || errB != nil || !os.SameFile(a, b) {
		return ""
	}
	return v
}

// firmlinkSpelling is the same directory reached through macOS's data-volume
// firmlink, when this host has one.
func firmlinkSpelling(p string) string {
	if runtime.GOOS != "darwin" {
		return ""
	}
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		return ""
	}
	fl := "/System/Volumes/Data" + resolved
	if _, err := os.Stat(fl); err != nil {
		return ""
	}
	return fl
}

// fixedDigester stands in for images.TreeDigest where only the cache's own
// bookkeeping is under test.
func fixedDigester(digest string) basecache.TreeDigester {
	return func(string) (string, error) { return digest, nil }
}

// A cache an older mgit composed at a root nobody names carries no marker,
// and no known root is it. Its entries still have the one shape this cache
// gives them, sha256/<64 hex>, and that layout is recognized at any root.
// Refs: MGIT-226
func TestOwner_AnEntryOfAPreMarkerCacheAnywhere_IsFoundByItsLayout(t *testing.T) {
	root := t.TempDir()
	hexName := strings.Repeat("e", 64)
	entry := filepath.Join(root, "sha256", hexName)
	require.NoError(t, os.MkdirAll(filepath.Join(entry, "usr", "bin"), 0o750))

	resolvedRoot, err := filepath.EvalSymlinks(root)
	require.NoError(t, err)
	for name, dir := range map[string]string{"the entry": entry, "a directory inside it": filepath.Join(entry, "usr", "bin")} {
		got, err := basecache.Owner(dir, nil)
		require.NoError(t, err, name)
		assert.Equal(t, resolvedRoot, got, "%s is inside the cache rooted at %s", name, root)
	}
	notAnEntry := filepath.Join(t.TempDir(), "sha256", "not-a-digest")
	require.NoError(t, os.MkdirAll(notAnEntry, 0o750))
	got, err := basecache.Owner(notAnEntry, nil)
	require.NoError(t, err)
	assert.Empty(t, got, "a directory merely named sha256 is not a cache")
}
