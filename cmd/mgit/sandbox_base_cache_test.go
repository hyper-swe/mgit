package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/sandboxd/basecache"
	"github.com/hyper-swe/mgit/internal/sandboxd/guestbase"
	"github.com/hyper-swe/mgit/internal/sandboxd/images"
)

// MGIT-147: the guest base is composed into a machine-wide, content-addressed
// cache. The repository keeps a digest and no bytes, and a recompose publishes
// a NEW entry rather than rewriting one.

// testBaseCache opens the cache this test was pointed at by newRepo.
func testBaseCache(t *testing.T) *basecache.Cache {
	t.Helper()
	root := os.Getenv(basecache.EnvRoot)
	require.NotEmpty(t, root, "newRepo must point the test at its own base cache")
	cache, err := basecache.New(root)
	require.NoError(t, err)
	return cache
}

// cachedBaseDir is where this repo's registered base actually lives.
func cachedBaseDir(t *testing.T, repo string) string {
	t.Helper()
	entry, err := images.LookupEntry(filepath.Join(repo, ".mgit", "sandbox"), defaultGuestBaseName)
	require.NoError(t, err)
	require.Empty(t, entry.RootfsPath, "a composed base must be located by digest, not by a stored path")
	path, err := testBaseCache(t).Path(entry.Digest)
	require.NoError(t, err)
	return path
}

// composeInto composes a base from a served image and returns the parsed JSON.
func composeInto(t *testing.T, repo, ref string) map[string]any {
	t.Helper()
	out, err := runBase(t, repo, "from", ref,
		"--guest-bin-dir", fakeGuestBins(t), "--plain-http", "--json")
	require.NoError(t, err, "base from: %s", out)
	_, body, ok := strings.Cut(out, "{")
	require.True(t, ok, "no JSON in output %q", out)
	var doc map[string]any
	require.NoError(t, json.Unmarshal([]byte("{"+body), &doc), "output was %q", out)
	return doc
}

// THE REPOSITORY MUST HOLD A REFERENCE AND NO BYTES.
//
// A 906 MB guest base unpacked into .mgit/sandbox/base is what a pilot's first
// provisioning produced, and it is what every host test command that walks the
// tree then had to walk. Refs: MGIT-147
func TestSandboxBaseFrom_LeavesNoBaseBytesInTheRepository(t *testing.T) {
	srv, ref := fakeImageServer(t, map[string]string{
		"bin/sh": "#!/bin/sh", "etc/os-release": "ID=debian",
		"usr/lib/x86_64-linux-gnu/libc.so.6": "glibc",
	})
	defer srv.Close()

	repo := newRepo(t)
	_, err := initTrustRoot(t, repo)
	require.NoError(t, err)
	doc := composeInto(t, repo, ref)

	assert.NoDirExists(t, inTreeBasePath(repo),
		"the composed base must not be unpacked inside the repository")
	// The bytes are in the cache, and the lock names them by digest alone.
	cachePath, _ := doc["cache_path"].(string)
	assert.FileExists(t, filepath.Join(cachePath, "bin", "sh"))
	assert.True(t, strings.HasPrefix(cachePath, os.Getenv(basecache.EnvRoot)),
		"the base must live in the machine-wide cache, got %q", cachePath)

	entry, err := images.LookupEntry(filepath.Join(repo, ".mgit", "sandbox"), defaultGuestBaseName)
	require.NoError(t, err)
	assert.Empty(t, entry.RootfsPath, "the repo must hold a digest, not a path into a copy")
	assert.Equal(t, doc["base_digest"], entry.Digest)
}

// GO-AGENT'S ACTUAL SYMPTOM: `gofmt -l .` in the host repository red-lit on
// 757 files that were mgit's artifact, not theirs — because a golang base
// image ships Go source, and mgit had unpacked it into their tree.
//
// This asserts their case rather than an approximation: a base image carrying
// deliberately misformatted Go, composed, and then the repo's own test command
// run over the repo. Refs: MGIT-147
func TestSandboxBaseFrom_AHostTestCommandWalkingTheRepo_SeesNoMgitArtifact(t *testing.T) {
	gofmt := findGofmt(t)
	srv, ref := fakeImageServer(t, map[string]string{
		"bin/sh": "#!/bin/sh",
		// What a golang:1.26-bookworm base actually carries: Go source, in
		// whatever shape upstream left it.
		"usr/local/go/src/fmt/print.go":     "package fmt\nfunc  Bad( ){\n}\n",
		"usr/local/go/src/net/http/http.go": "package http\nvar   X   =   1\n",
	})
	defer srv.Close()

	repo := newRepo(t)
	_, err := initTrustRoot(t, repo)
	require.NoError(t, err)
	// The repository's own source, correctly formatted.
	require.NoError(t, os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\n"), 0o600))

	composeInto(t, repo, ref)

	//nolint:gosec // G204: gofmt path resolved from the toolchain, repo is a test temp dir
	flagged, _ := exec.Command(gofmt, "-l", repo).CombinedOutput()
	assert.Empty(t, strings.TrimSpace(string(flagged)),
		"a host test command walking the repo saw mgit's artifact:\n%s", flagged)
}

// movableTagImageServer serves an image whose tag can be re-pointed at
// different bytes while the server keeps its address — the only faithful way
// to reproduce "the digest changed under the same tag". Refs: MGIT-147
func movableTagImageServer(t *testing.T, files map[string]string) (*httptest.Server, string, func(map[string]string)) {
	t.Helper()
	arch := "amd64"
	if runtime.GOARCH == "arm64" {
		arch = "arm64"
	}
	var (
		mu      sync.Mutex
		content = buildImageContent(t, files, "", arch)
	)
	srv := httptest.NewServer(imageMux(func() imageContent {
		mu.Lock()
		defer mu.Unlock()
		return content
	}))
	move := func(next map[string]string) {
		built := buildImageContent(t, next, "", arch)
		mu.Lock()
		defer mu.Unlock()
		content = built
	}
	return srv, strings.TrimPrefix(srv.URL, "http://") + "/acme/base:v1", move
}

// findGofmt locates gofmt from PATH or the active toolchain.
func findGofmt(t *testing.T) string {
	t.Helper()
	if path, err := exec.LookPath("gofmt"); err == nil {
		return path
	}
	//nolint:noctx // one-shot toolchain query in a test
	out, err := exec.Command("go", "env", "GOROOT").Output()
	if err != nil {
		t.Skip("no gofmt and no go toolchain to find one with")
	}
	path := filepath.Join(strings.TrimSpace(string(out)), "bin", "gofmt")
	if _, statErr := os.Stat(path); statErr != nil {
		t.Skip("no gofmt available")
	}
	return path
}

// THE REGRESSION THAT MOTIVATED THE TICKET.
//
// Recomposing a base in one worktree used to rewrite the ONE directory every
// worktree on the machine shared, so another lane's pinned digest stopped
// matching its bytes — this lane lost two live verification runs to exactly
// that. A recompose must now publish a new entry and leave the old one alone.
// Refs: MGIT-147
func TestSandboxBaseFrom_RecomposingDoesNotDisturbTheBaseAnotherLanePinned(t *testing.T) {
	first, firstURL := fakeImageServer(t, map[string]string{
		"bin/sh": "#!/bin/sh", "etc/os-release": "ID=debian-v1",
	})
	defer first.Close()
	second, secondURL := fakeImageServer(t, map[string]string{
		"bin/sh": "#!/bin/sh", "etc/os-release": "ID=debian-v2",
	})
	defer second.Close()

	repo := newRepo(t)
	_, err := initTrustRoot(t, repo)
	require.NoError(t, err)

	pinned := composeInto(t, repo, firstURL)
	pinnedDigest, _ := pinned["base_digest"].(string)
	pinnedPath, _ := pinned["cache_path"].(string)

	// Another lane recomposes from a different image.
	recomposed := composeInto(t, repo, secondURL)
	assert.NotEqual(t, pinnedDigest, recomposed["base_digest"])
	assert.NotEqual(t, pinnedPath, recomposed["cache_path"],
		"a recompose must publish a new entry, never rewrite one")

	// The base the first lane pinned is untouched, and still hashes to the
	// digest it was pinned under — the check that failed twice, live.
	got, err := images.TreeDigest(pinnedPath)
	require.NoError(t, err)
	assert.Equal(t, pinnedDigest, got,
		"recomposing invalidated the digest another lane had pinned")
	content, err := os.ReadFile(filepath.Join(pinnedPath, "etc", "os-release")) //nolint:gosec // test temp path
	require.NoError(t, err)
	assert.Equal(t, "ID=debian-v1", string(content))
}

// A TAG IS A NAME THAT CAN POINT TWICE.
//
// golang:1.26-bookworm resolved to two different images a day apart, and with
// the tag as identity nobody could say whether upstream had moved or our
// composition had changed. Re-composing the same tag onto different bytes must
// therefore produce a NEW entry, keep the old one, and SAY so. Refs: MGIT-147
func TestSandboxBaseFrom_ATagThatMoved_AddsAnEntryAndSaysWhatChanged(t *testing.T) {
	repo := newRepo(t)
	_, err := initTrustRoot(t, repo)
	require.NoError(t, err)

	srv, ref, moveTag := movableTagImageServer(t, map[string]string{
		"bin/sh": "#!/bin/sh", "etc/os-release": "ID=debian-day-one",
	})
	defer srv.Close()
	firstDoc := composeInto(t, repo, ref)

	// Day two: the SAME tag, at the same registry, now serves different bytes.
	moveTag(map[string]string{"bin/sh": "#!/bin/sh", "etc/os-release": "ID=debian-day-two"})

	out, err := runBase(t, repo, "from", ref,
		"--guest-bin-dir", fakeGuestBins(t), "--plain-http")
	require.NoError(t, err, "base from: %s", out)

	assert.Contains(t, out, "now resolves to a different image",
		"a moved tag must be reported, not absorbed silently:\n%s", out)
	assert.Contains(t, out, firstDoc["base_digest"].(string),
		"the report must name the base that was superseded")

	// The superseded entry is still there for anything that pinned it.
	oldPath, _ := firstDoc["cache_path"].(string)
	assert.DirExists(t, oldPath, "the superseded base must not be deleted")

	// And the journal records both, oldest first — images.lock cannot, since
	// it holds one entry per name.
	history, err := guestbase.ComposeHistory(filepath.Join(repo, ".mgit", "sandbox"))
	require.NoError(t, err)
	require.Len(t, history, 2)
	assert.Equal(t, firstDoc["base_digest"], history[0].BaseDigest)
	assert.True(t, history[1].TagMoved(), "the second compose must be journalled as a moved tag")
	assert.Equal(t, history[0].SourceRef, history[1].PrevSourceRef)
}

// MIGRATION: an existing checkout has an in-tree base. It is MOVED into the
// cache — not ignored, not silently deleted — and the move is announced,
// because bytes changing place without explanation is how trust is lost.
// The digest is preserved, so what was pinned still resolves. Refs: MGIT-147
func TestSandboxBase_WithAnInTreeBaseFromAnOlderMgit_MovesItOutAndSaysSo(t *testing.T) {
	repo := newRepo(t)
	hostRoot := filepath.Join(repo, ".mgit", "sandbox")
	_, err := initTrustRoot(t, repo)
	require.NoError(t, err)

	// Exactly what a pre-MGIT-147 compose left behind: a base tree inside the
	// repo, pinned into images.lock by its path.
	legacy := inTreeBasePath(repo)
	for _, dir := range guestBaseMountDirs {
		require.NoError(t, os.MkdirAll(filepath.Join(legacy, dir), 0o750))
	}
	require.NoError(t, os.MkdirAll(filepath.Join(legacy, "bin"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(legacy, "bin", "sh"), []byte("#!/bin/sh"), 0o600))
	legacyEntry, err := images.BuildBaseEntry(legacy)
	require.NoError(t, err)
	priv, err := images.LoadSigningKey(hostRoot)
	require.NoError(t, err)
	pinnedRef, err := images.Register(hostRoot, defaultGuestBaseName, legacyEntry, priv)
	require.NoError(t, err)

	// A launch is the first sandbox command a user runs after upgrading, and
	// it resolves this repo's base — so that is where the migration happens.
	t.Chdir(repo)
	out, err := runSandbox(okConnect(&fakeSandboxClient{}), "launch",
		"--task", "MGIT-147.1", "--worktree", filepath.Join(t.TempDir(), "wt"))
	require.NoError(t, err, "launch: %s", out)

	assert.Contains(t, out, "out of the repository", "the migration must be announced:\n%s", out)
	assert.NoDirExists(t, legacy, "the in-tree base must be gone after migration")

	// The pin is untouched and now resolves out of the cache.
	cache := testBaseCache(t)
	store, err := images.NewStoreWithBaseCache(hostRoot, time.Now, cache)
	require.NoError(t, err)
	resolved, err := store.Resolve(pinnedRef)
	require.NoError(t, err, "migration must preserve the pin")
	assert.FileExists(t, filepath.Join(resolved.RootfsPath, "bin", "sh"))
}

// `base set` is the bring-your-own-tree path: mgit pins the user's directory
// rather than a copy. A tree INSIDE the repository would reintroduce exactly
// the artifact this ticket removes, so it is refused with the reason.
// Refs: MGIT-147
func TestSandboxBaseSet_ATreeInsideTheRepository_IsRefusedWithTheReason(t *testing.T) {
	repo := newRepo(t)
	_, err := initTrustRoot(t, repo)
	require.NoError(t, err)

	inRepo := filepath.Join(repo, "vendor", "guest-base")
	for _, dir := range append([]string{"bin"}, guestBaseMountDirs...) {
		require.NoError(t, os.MkdirAll(filepath.Join(inRepo, dir), 0o750))
	}
	require.NoError(t, os.WriteFile(filepath.Join(inRepo, "bin", "sh"), []byte("#!/bin/sh"), 0o600))

	out, err := runBase(t, repo, "set", inRepo, "--guest-bin-dir", fakeGuestBins(t))

	require.Error(t, err, "an in-repo base tree must be refused, got %q", out)
	msg := err.Error()
	assert.Contains(t, msg, "inside the repository")
	assert.Contains(t, msg, "gofmt -l .", "the refusal must name the symptom it prevents")
	assert.Contains(t, msg, "mgit sandbox base from", "the refusal must name the alternative")
}

// Composing the same image twice must reuse the cached entry rather than
// unpacking it again — the second-order win of content addressing, and the
// reason N repos on a machine stop paying N copies. Refs: MGIT-147
func TestSandboxBaseFrom_RecomposingTheSameImage_ReusesTheCachedEntry(t *testing.T) {
	srv, ref := fakeImageServer(t, map[string]string{"bin/sh": "#!/bin/sh"})
	defer srv.Close()

	repo := newRepo(t)
	_, err := initTrustRoot(t, repo)
	require.NoError(t, err)

	first := composeInto(t, repo, ref)
	second := composeInto(t, repo, ref)

	assert.Equal(t, first["base_digest"], second["base_digest"])
	assert.Equal(t, first["cache_path"], second["cache_path"])
	assert.Equal(t, false, first["reused"])
	assert.Equal(t, true, second["reused"], "identical bytes must be reused, not re-unpacked")
}
