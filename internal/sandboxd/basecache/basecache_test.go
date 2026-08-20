package basecache_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/sandboxd/basecache"
	"github.com/hyper-swe/mgit/internal/sandboxd/images"
)

// treeWith builds a small tree under a fresh staging dir of the cache.
func treeWith(t *testing.T, cache *basecache.Cache, files map[string]string) string {
	t.Helper()
	staging, err := cache.Stage()
	require.NoError(t, err)
	writeTree(t, staging, files)
	return staging
}

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(root, name)
		//nolint:gosec // G703: root and names are test-owned literals, not external input
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
		//nolint:gosec // G703: as above
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	}
}

func newCache(t *testing.T) *basecache.Cache {
	t.Helper()
	cache, err := basecache.New(t.TempDir())
	require.NoError(t, err)
	return cache
}

func TestDefaultRoot_WithEnvOverride_UsesIt(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(basecache.EnvRoot, dir)

	root, err := basecache.DefaultRoot()
	require.NoError(t, err)
	assert.Equal(t, dir, root, "the env override must be honored verbatim")
}

func TestDefaultRoot_WithoutEnvOverride_LivesUnderTheOSCacheDir(t *testing.T) {
	t.Setenv(basecache.EnvRoot, "")

	root, err := basecache.DefaultRoot()
	require.NoError(t, err)

	osCache, err := os.UserCacheDir()
	require.NoError(t, err)
	// XDG on Linux (os.UserCacheDir reads XDG_CACHE_HOME), ~/Library/Caches
	// on macOS. Either way it is OUT of any repository and shared by every
	// repo and agent on the machine. Refs: MGIT-147
	assert.Equal(t, filepath.Join(osCache, "mgit", "bases"), root)
}

func TestDefaultRoot_WithRelativeEnvOverride_IsRefused(t *testing.T) {
	t.Setenv(basecache.EnvRoot, "relative/cache")

	_, err := basecache.DefaultRoot()
	require.Error(t, err, "a relative cache root would move with the working directory")
	assert.Contains(t, err.Error(), "absolute")
}

func TestCommit_PublishesUnderItsDigest_AndLeavesNoStaging(t *testing.T) {
	cache := newCache(t)
	staging := treeWith(t, cache, map[string]string{"bin/sh": "#!/bin/sh"})

	entry, err := cache.Commit(staging, images.TreeDigest)
	require.NoError(t, err)

	// Identity IS the path: the entry lives at the digest that names it.
	assert.Equal(t, entry.Path, mustPath(t, cache, entry.Digest))
	assert.FileExists(t, filepath.Join(entry.Path, "bin", "sh"))
	assert.NoDirExists(t, staging, "the staging tree must be consumed by the commit")

	// And the published bytes hash to the name they were filed under.
	got, err := images.TreeDigest(entry.Path)
	require.NoError(t, err)
	assert.Equal(t, entry.Digest, got)
}

func TestCommit_SameDigestTwice_KeepsOneEntryAndDeduplicates(t *testing.T) {
	cache := newCache(t)
	files := map[string]string{"bin/sh": "#!/bin/sh"}

	first, err := cache.Commit(treeWith(t, cache, files), images.TreeDigest)
	require.NoError(t, err)
	second, err := cache.Commit(treeWith(t, cache, files), images.TreeDigest)
	require.NoError(t, err)

	assert.Equal(t, first.Digest, second.Digest)
	assert.Equal(t, first.Path, second.Path)
	assert.False(t, first.Deduplicated, "the first population is not a dedupe")
	assert.True(t, second.Deduplicated, "identical bytes must reuse the entry, not republish it")
}

// A recompose that produces DIFFERENT bytes must produce a NEW entry beside
// the old one — never overwrite it. This is the property that makes one
// worktree unable to invalidate another's pin. Refs: MGIT-147, MGIT-105
func TestCommit_DifferentBytes_AddsAnEntryRatherThanReplacingOne(t *testing.T) {
	cache := newCache(t)

	old, err := cache.Commit(treeWith(t, cache, map[string]string{"bin/sh": "v1"}), images.TreeDigest)
	require.NoError(t, err)
	fresh, err := cache.Commit(treeWith(t, cache, map[string]string{"bin/sh": "v2"}), images.TreeDigest)
	require.NoError(t, err)

	require.NotEqual(t, old.Digest, fresh.Digest)
	assert.DirExists(t, old.Path, "the superseded base must still be there for whoever pinned it")
	content, err := os.ReadFile(filepath.Join(old.Path, "bin", "sh")) //nolint:gosec // test-owned path
	require.NoError(t, err)
	assert.Equal(t, "v1", string(content), "the old entry's bytes changed under it")
}

// Two populations of the SAME digest racing each other must leave exactly one
// entry, complete and verifiable — never a half-written tree. Refs: MGIT-147
func TestCommit_ConcurrentPopulationOfTheSameDigest_YieldsOneValidEntry(t *testing.T) {
	cache := newCache(t)
	files := map[string]string{
		"bin/sh":         "#!/bin/sh",
		"usr/lib/libc.a": strings.Repeat("x", 4096),
		"etc/os-release": "ID=debian",
	}

	const racers = 8
	stagings := make([]string, racers)
	for i := range stagings {
		stagings[i] = treeWith(t, cache, files)
	}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		entries []basecache.Entry
		errs    []error
	)
	start := make(chan struct{})
	for i := range racers {
		wg.Add(1)
		go func(staging string) {
			defer wg.Done()
			<-start
			entry, err := cache.Commit(staging, images.TreeDigest)
			mu.Lock()
			defer mu.Unlock()
			entries = append(entries, entry)
			errs = append(errs, err)
		}(stagings[i])
	}
	close(start)
	wg.Wait()

	published := 0
	for i, err := range errs {
		require.NoError(t, err, "racer %d", i)
		assert.Equal(t, entries[0].Digest, entries[i].Digest)
		if !entries[i].Deduplicated {
			published++
		}
	}
	assert.Equal(t, 1, published, "exactly one racer may publish; the rest must dedupe")

	// The single surviving entry must hash to its own name — a torn tree
	// would not.
	got, err := images.TreeDigest(entries[0].Path)
	require.NoError(t, err)
	assert.Equal(t, entries[0].Digest, got, "the published entry does not match its digest")

	// And no debris: every staging tree is gone.
	for _, staging := range stagings {
		assert.NoDirExists(t, staging)
	}
}

func TestPath_MalformedDigest_IsRefused(t *testing.T) {
	cache := newCache(t)
	tests := []struct {
		name   string
		digest string
	}{
		{name: "empty", digest: ""},
		{name: "no_algorithm", digest: strings.Repeat("a", 64)},
		{name: "wrong_algorithm", digest: "sha1:" + strings.Repeat("a", 40)},
		{name: "short_hex", digest: "sha256:abc"},
		{name: "not_hex", digest: "sha256:" + strings.Repeat("z", 64)},
		{name: "path_traversal", digest: "sha256:../../../etc"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := cache.Path(tt.digest)
			require.Error(t, err, "a malformed digest must never become a path")
		})
	}
}

func TestHas_MissingDigest_ReportsFalse(t *testing.T) {
	cache := newCache(t)
	assert.False(t, cache.Has("sha256:"+strings.Repeat("a", 64)))
	assert.False(t, cache.Has("not-a-digest"))
}

// Migration: an existing in-tree base is MOVED into the cache under the digest
// it already hashes to, so every pin that referenced it keeps resolving.
// Refs: MGIT-147
func TestAdopt_MovesATreeIn_PreservingItsDigest(t *testing.T) {
	cache := newCache(t)
	legacy := filepath.Join(t.TempDir(), "base")
	require.NoError(t, os.MkdirAll(legacy, 0o750))
	writeTree(t, legacy, map[string]string{"bin/sh": "#!/bin/sh", "etc/hosts": "127.0.0.1"})

	before, err := images.TreeDigest(legacy)
	require.NoError(t, err)

	entry, err := cache.Adopt(legacy, images.TreeDigest)
	require.NoError(t, err)

	assert.Equal(t, before, entry.Digest, "adoption must not change the digest, or every pin breaks")
	assert.NoDirExists(t, legacy, "the in-tree base must be gone after adoption")
	assert.FileExists(t, filepath.Join(entry.Path, "bin", "sh"))
}

func TestAdopt_WhenTheDigestIsAlreadyCached_DropsTheDuplicateBytes(t *testing.T) {
	cache := newCache(t)
	files := map[string]string{"bin/sh": "#!/bin/sh"}
	first, err := cache.Commit(treeWith(t, cache, files), images.TreeDigest)
	require.NoError(t, err)

	legacy := filepath.Join(t.TempDir(), "base")
	require.NoError(t, os.MkdirAll(legacy, 0o750))
	writeTree(t, legacy, files)

	entry, err := cache.Adopt(legacy, images.TreeDigest)
	require.NoError(t, err)
	assert.True(t, entry.Deduplicated)
	assert.Equal(t, first.Path, entry.Path)
	assert.NoDirExists(t, legacy)
}

func TestPruneStaging_RemovesAbandonedTreesAndKeepsFreshOnes(t *testing.T) {
	cache := newCache(t)
	stale := treeWith(t, cache, map[string]string{"a": "1"})
	fresh := treeWith(t, cache, map[string]string{"b": "2"})

	old := time.Now().Add(-48 * time.Hour)
	require.NoError(t, os.Chtimes(stale, old, old))

	removed, err := cache.PruneStaging(time.Now(), 24*time.Hour)
	require.NoError(t, err)
	assert.Equal(t, 1, removed)
	assert.NoDirExists(t, stale, "a crashed compose must not leak its staging tree forever")
	assert.DirExists(t, fresh, "a compose in flight must not be swept out from under itself")
}

func TestPruneStaging_WithNoCacheYet_IsANoOp(t *testing.T) {
	cache, err := basecache.New(filepath.Join(t.TempDir(), "never-created"))
	require.NoError(t, err)
	removed, err := cache.PruneStaging(time.Now(), time.Hour)
	require.NoError(t, err)
	assert.Zero(t, removed)
}

func TestNew_WithEmptyRoot_IsRefused(t *testing.T) {
	_, err := basecache.New("")
	require.Error(t, err)
}

func mustPath(t *testing.T, cache *basecache.Cache, digest string) string {
	t.Helper()
	path, err := cache.Path(digest)
	require.NoError(t, err)
	return path
}

// helperEnv marks a child process spawned by the two-process race test, and
// carries the cache root and start gate it should use.
const (
	helperEnv     = "MGIT_BASECACHE_HELPER"
	helperRootEnv = "MGIT_BASECACHE_HELPER_ROOT"
	helperGateEnv = "MGIT_BASECACHE_HELPER_GATE"
)

// TWO PROCESSES, NOT TWO GOROUTINES.
//
// Populating the same base from two mgit processes at once is the real case:
// two agents on one machine provisioning at the same moment, which is exactly
// the shape of defect this project keeps finding. Goroutines share a Cache and
// a heap; separate processes share only the filesystem, which is where the
// guarantee has to live. Refs: MGIT-147
func TestCommit_TwoProcessesPopulatingTheSameDigest_YieldOneValidEntry(t *testing.T) {
	if os.Getenv(helperEnv) != "" {
		t.Skip("child process; the helper test does the work")
	}
	cacheRoot := t.TempDir()
	gate := filepath.Join(t.TempDir(), "go")

	const children = 2
	outputs := make(chan string, children)
	errs := make(chan error, children)
	for range children {
		//nolint:gosec // G204: re-executing this test binary with a fixed -test.run
		child := exec.Command(os.Args[0], "-test.run=TestPopulateHelperProcess", "-test.v")
		child.Env = append(os.Environ(),
			helperEnv+"=1", helperRootEnv+"="+cacheRoot, helperGateEnv+"="+gate)
		go func() {
			out, err := child.CombinedOutput()
			outputs <- string(out)
			errs <- err
		}()
	}
	// Release both at once, so the two renames actually overlap.
	time.Sleep(150 * time.Millisecond)
	require.NoError(t, os.WriteFile(gate, []byte("go"), 0o600))

	published, deduped, digest := 0, 0, ""
	for range children {
		out := <-outputs
		require.NoError(t, <-errs, "child failed:\n%s", out)
		switch {
		case strings.Contains(out, "RESULT published "):
			published++
			digest = resultDigest(t, out)
		case strings.Contains(out, "RESULT deduped "):
			deduped++
			digest = resultDigest(t, out)
		default:
			t.Fatalf("child reported no result:\n%s", out)
		}
	}
	assert.Equal(t, 1, published, "exactly one process may publish the entry")
	assert.Equal(t, 1, deduped, "the other must find the bytes already there")

	// One entry, complete, hashing to its own name.
	cache, err := basecache.New(cacheRoot)
	require.NoError(t, err)
	path, err := cache.Path(digest)
	require.NoError(t, err)
	got, err := images.TreeDigest(path)
	require.NoError(t, err)
	assert.Equal(t, digest, got, "the surviving entry does not match its digest")

	// And nothing was left in staging.
	staging, err := os.ReadDir(filepath.Join(cacheRoot, "staging"))
	require.NoError(t, err)
	assert.Empty(t, staging, "a completed race must leave no staging debris")
}

// resultDigest pulls the digest out of a child's RESULT line.
func resultDigest(t *testing.T, out string) string {
	t.Helper()
	for line := range strings.SplitSeq(out, "\n") {
		if _, rest, ok := strings.Cut(line, "RESULT "); ok {
			fields := strings.Fields(rest)
			require.Len(t, fields, 2, "malformed result line %q", line)
			return fields[1]
		}
	}
	t.Fatalf("no RESULT line in:\n%s", out)
	return ""
}

// TestPopulateHelperProcess is the child half of the two-process race: it
// composes an identical tree and reports whether it published or deduped.
func TestPopulateHelperProcess(t *testing.T) {
	if os.Getenv(helperEnv) == "" {
		t.Skip("not a spawned helper process")
	}
	cache, err := basecache.New(os.Getenv(helperRootEnv))
	require.NoError(t, err)
	staging, err := cache.Stage()
	require.NoError(t, err)
	// Big enough that the copy is not instantaneous, so the renames overlap.
	files := map[string]string{}
	for i := range 64 {
		files[fmt.Sprintf("usr/lib/blob-%02d", i)] = strings.Repeat("x", 8192)
	}
	files["bin/sh"] = "#!/bin/sh"
	writeTree(t, staging, files)

	waitForGate(t, os.Getenv(helperGateEnv))

	entry, err := cache.Commit(staging, images.TreeDigest)
	require.NoError(t, err)
	state := "published"
	if entry.Deduplicated {
		state = "deduped"
	}
	fmt.Printf("RESULT %s %s\n", state, entry.Digest)
}

// waitForGate blocks until the parent releases the racers.
func waitForGate(t *testing.T, gate string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		//nolint:gosec // G703: the gate path is handed to this child by the parent test
		if _, err := os.Stat(gate); err == nil {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("the start gate never opened")
}
