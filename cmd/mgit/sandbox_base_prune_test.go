package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/sandboxd/basecache"
	"github.com/hyper-swe/mgit/internal/sandboxd/baseprune"
	"github.com/hyper-swe/mgit/internal/sandboxd/daemonrec"
	"github.com/hyper-swe/mgit/internal/sandboxd/images"
)

// MGIT-239: every pin leaves a back-reference beside the cache, and `base
// prune` removes only what no recorded repository pins any more.

// pinnersOf is the resolved host roots recorded as pinning a repo's base.
func pinnersOf(t *testing.T, repo string) []string {
	t.Helper()
	entry, err := images.LookupEntry(filepath.Join(repo, ".mgit", "sandbox"), defaultGuestBaseName)
	require.NoError(t, err)
	roots, _, err := testBaseCache(t).Pinners(entry.Digest)
	require.NoError(t, err)
	return roots
}

func hostRootOf(t *testing.T, repo string) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(filepath.Join(repo, ".mgit", "sandbox"))
	require.NoError(t, err)
	return root
}

func TestSandboxBaseFrom_RecordsThisRepositoryAsAPinner(t *testing.T) {
	srv, ref := fakeImageServer(t, map[string]string{"bin/sh": "#!/bin/sh", "etc/os-release": "ID=debian"})
	defer srv.Close()
	repo := newRepo(t)
	_, err := initTrustRoot(t, repo)
	require.NoError(t, err)

	composeInto(t, repo, ref)

	assert.Equal(t, []string{hostRootOf(t, repo)}, pinnersOf(t, repo))
}

// A repository that pinned an entry before back-references existed gets one
// the next time it launches: launch resolves the pin, and records it.
func TestSandboxLaunch_RecordsThisRepositoryAsAPinner(t *testing.T) {
	srv, ref := fakeImageServer(t, map[string]string{"bin/sh": "#!/bin/sh", "etc/os-release": "ID=debian"})
	defer srv.Close()
	repo := newRepo(t)
	_, err := initTrustRoot(t, repo)
	require.NoError(t, err)
	composeInto(t, repo, ref)
	require.NoError(t, os.RemoveAll(filepath.Join(testBaseCache(t).Root(), "pinners")),
		"stand in for an entry composed before pinners were recorded")
	require.Empty(t, pinnersOf(t, repo))

	t.Chdir(repo)
	out, err := runSandbox(okConnect(&fakeSandboxClient{}), "launch",
		"--task", "MGIT-239.1", "--worktree", filepath.Join(t.TempDir(), "wt"))
	require.NoError(t, err, "launch: %s", out)

	assert.Equal(t, []string{hostRootOf(t, repo)}, pinnersOf(t, repo))
}

func TestSandboxBase_MigratedInTreeBase_RecordsThisRepositoryAsAPinner(t *testing.T) {
	repo := newRepo(t)
	hostRoot := filepath.Join(repo, ".mgit", "sandbox")
	_, err := initTrustRoot(t, repo)
	require.NoError(t, err)
	legacy := inTreeBasePath(repo)
	for _, dir := range append([]string{"bin"}, guestBaseMountDirs...) {
		require.NoError(t, os.MkdirAll(filepath.Join(legacy, dir), 0o750))
	}
	require.NoError(t, os.WriteFile(filepath.Join(legacy, "bin", "sh"), []byte("#!/bin/sh"), 0o600))
	legacyEntry, err := images.BuildBaseEntry(legacy)
	require.NoError(t, err)
	priv, err := images.LoadSigningKey(hostRoot)
	require.NoError(t, err)
	_, err = images.Register(hostRoot, defaultGuestBaseName, legacyEntry, priv)
	require.NoError(t, err)
	cache, err := openBaseCache()
	require.NoError(t, err)

	require.NoError(t, migrateInTreeBase(hostRoot, cache, &bytes.Buffer{}))

	roots, recorded, err := cache.Pinners(legacyEntry.Digest)
	require.NoError(t, err)
	assert.True(t, recorded)
	assert.Equal(t, []string{hostRootOf(t, repo)}, roots)
}

// testPruneOpen wires `base prune` to this test's cache, the real lock
// reader, and a fixed daemon answer — never to this machine's daemons.
func testPruneOpen(t *testing.T, inUse map[string]bool, asked error) func() (baseprune.Deps, error) {
	return func() (baseprune.Deps, error) {
		return baseprune.Deps{
			Cache:    testBaseCache(t),
			LockPins: images.CachedPins,
			InUse:    func(context.Context) (baseprune.Live, error) { return baseprune.Live{Digests: inUse}, asked },
		}, nil
	}
}

func runPrune(t *testing.T, open func() (baseprune.Deps, error), args ...string) (string, error) {
	t.Helper()
	cmd := newSandboxBasePruneCmd(open)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(t.Context())
	return out.String(), err
}

// THE TICKET'S SCENARIO, through the verb. Two repositories compose two
// bases; one recomposes from another image; prune removes only the base
// nobody pins, and the other repository still launches. Refs: MGIT-239
func TestSandboxBasePrune_OneRepoRecomposes_RemovesOnlyTheOrphan(t *testing.T) {
	srvX, refX := fakeImageServer(t, map[string]string{"bin/sh": "#!/bin/sh", "etc/os-release": "ID=x"})
	defer srvX.Close()
	srvY, refY := fakeImageServer(t, map[string]string{"bin/sh": "#!/bin/sh", "etc/os-release": "ID=y"})
	defer srvY.Close()
	srvZ, refZ := fakeImageServer(t, map[string]string{"bin/sh": "#!/bin/sh", "etc/os-release": "ID=z"})
	defer srvZ.Close()
	a, b := newRepo(t), newRepo(t)
	for _, r := range []string{a, b} {
		_, err := initTrustRoot(t, r)
		require.NoError(t, err)
	}
	digestX := composeInto(t, a, refX)["base_digest"].(string)
	digestY := composeInto(t, b, refY)["base_digest"].(string)
	digestZ := composeInto(t, b, refZ)["base_digest"].(string)
	cache := testBaseCache(t)
	open := testPruneOpen(t, map[string]bool{}, nil)

	plan, err := runPrune(t, open, "--dry-run")
	require.NoError(t, err)
	assert.Contains(t, plan, "1 prunable", plan)
	assert.Contains(t, plan, "re-pinned", "the plan names what each pinner's lock says now:\n%s", plan)
	assert.True(t, cache.Has(digestY), "a dry run removes nothing")

	out, err := runPrune(t, open)
	require.NoError(t, err, out)
	assert.Contains(t, out, "removed 1, freed", out)
	assert.False(t, cache.Has(digestY))
	assert.True(t, cache.Has(digestX))
	assert.True(t, cache.Has(digestZ))

	t.Chdir(a)
	launched, err := runSandbox(okConnect(&fakeSandboxClient{}), "launch",
		"--task", "MGIT-239.2", "--worktree", filepath.Join(t.TempDir(), "wt"))
	require.NoError(t, err, "the repository whose pin survived must still launch: %s", launched)
	store, err := images.NewStoreWithBaseCache(filepath.Join(a, ".mgit", "sandbox"), time.Now, cache)
	require.NoError(t, err)
	_, err = store.Resolve(defaultGuestBaseName + "@" + digestX)
	require.NoError(t, err)
}

func TestSandboxBasePrune_DaemonSilent_RemovesNothingAndSaysWhy(t *testing.T) {
	srvY, refY := fakeImageServer(t, map[string]string{"bin/sh": "#!/bin/sh", "etc/os-release": "ID=y"})
	defer srvY.Close()
	srvZ, refZ := fakeImageServer(t, map[string]string{"bin/sh": "#!/bin/sh", "etc/os-release": "ID=z"})
	defer srvZ.Close()
	b := newRepo(t)
	_, err := initTrustRoot(t, b)
	require.NoError(t, err)
	digestY := composeInto(t, b, refY)["base_digest"].(string)
	composeInto(t, b, refZ)

	out, err := runPrune(t, testPruneOpen(t, nil, errors.New("the daemon for /r (pid 7) did not answer")))
	require.NoError(t, err, out)
	assert.Contains(t, out, "removed 0", out)
	assert.Contains(t, out, "did not answer", out)
	assert.True(t, testBaseCache(t).Has(digestY))
}

func TestSandboxBasePrune_DryRunWithRemoveUnknown_Refused(t *testing.T) {
	newRepo(t)
	_, err := runPrune(t, testPruneOpen(t, nil, nil), "--dry-run", "--remove-unknown",
		"sha256:0000000000000000000000000000000000000000000000000000000000000000")
	require.Error(t, err)
}

func TestSandboxBasePrune_EmptyCache_SaysSo(t *testing.T) {
	newRepo(t)
	out, err := runPrune(t, testPruneOpen(t, nil, nil), "--dry-run")
	require.NoError(t, err)
	assert.Contains(t, out, "holds no entries")
}

func TestLiveSandboxDigests_AsksLiveDaemonsOnly(t *testing.T) {
	list := func(context.Context) ([]daemonrec.Listed, error) {
		return []daemonrec.Listed{{
			Record: daemonrec.Record{PID: 1, RepoRoot: "/gone", Socket: filepath.Join(t.TempDir(), "none.sock")},
			Status: daemonrec.Status{Alive: false},
		}}, nil
	}
	live, err := liveSandboxes(t.Context(), list)
	require.NoError(t, err, "a dead daemon runs nothing and is not asked")
	assert.Empty(t, live.Digests)
	assert.Empty(t, live.HostRoots)
}

func TestLiveSandboxDigests_LiveDaemonThatCannotBeAsked_IsAnError(t *testing.T) {
	list := func(context.Context) ([]daemonrec.Listed, error) {
		return []daemonrec.Listed{{
			Record: daemonrec.Record{PID: os.Getpid(), RepoRoot: "/r", Socket: filepath.Join(t.TempDir(), "none.sock")},
			Status: daemonrec.Status{Alive: true},
		}}, nil
	}
	_, err := liveSandboxes(t.Context(), list)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "/r")
}

func TestLiveSandboxDigests_ListFails_IsAnError(t *testing.T) {
	list := func(context.Context) ([]daemonrec.Listed, error) { return nil, errors.New("unreadable record") }
	_, err := liveSandboxes(t.Context(), list)
	require.Error(t, err)
}

func TestOccupiesBase_OnlyFinishedSandboxesReleaseIt(t *testing.T) {
	for state, want := range map[string]bool{
		"created": true, "running": true, "suspended": true,
		"landed": false, "destroyed": false, "dead": false,
	} {
		assert.Equal(t, want, occupiesBase(state), state)
	}
}

// A dry run promises to change nothing, so opening prune's view of the cache
// must not sweep it: staging debris, however old, is still there afterwards.
func TestHostPruneDeps_OpeningTheCache_RemovesNoStagingDebris(t *testing.T) {
	root := t.TempDir()
	t.Setenv(basecache.EnvRoot, root)
	debris := filepath.Join(root, "staging", "compose-old")
	require.NoError(t, os.MkdirAll(debris, 0o750))
	old := time.Now().Add(-48 * time.Hour)
	require.NoError(t, os.Chtimes(debris, old, old))

	d, err := hostPruneDeps()
	require.NoError(t, err)

	assert.Equal(t, root, d.Cache.Root())
	assert.DirExists(t, debris, "opening the cache for prune must remove nothing")
}

// A daemon's refusal can run to a page (a wire-version mismatch prints the
// whole upgrade procedure). The verdict repeats the reason on every entry, so
// only its first line is carried: the table stays one line per reason.
func TestLiveSandboxDigests_ADaemonsLongRefusal_IsCarriedAsItsFirstLine(t *testing.T) {
	err := daemonSilence(daemonrec.Record{PID: 7, RepoRoot: "/r"},
		errors.New("mgit CLI and daemon differ — upgrade both.\n  mgit CLI: protocol 5\n  daemon: protocol 4"))
	assert.Equal(t, "the daemon for /r (pid 7) did not answer: mgit CLI and daemon differ — upgrade both.", err.Error())
}

// A compose that cannot record its pin pins nothing: the record comes before
// the lock is signed, so no repository can pin an entry unrecorded.
func TestSandboxBaseFrom_RecordFails_PinsNothing(t *testing.T) {
	srv, ref := fakeImageServer(t, map[string]string{"bin/sh": "#!/bin/sh", "etc/os-release": "ID=debian"})
	defer srv.Close()
	repo := newRepo(t)
	_, err := initTrustRoot(t, repo)
	require.NoError(t, err)
	root := testBaseCache(t).Root()
	require.NoError(t, os.MkdirAll(root, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(root, "pinners"), nil, 0o600), "block every record")

	out, err := runBase(t, repo, "from", ref, "--guest-bin-dir", fakeGuestBins(t), "--plain-http")
	require.Error(t, err, out)
	_, lookErr := images.LookupEntry(filepath.Join(repo, ".mgit", "sandbox"), defaultGuestBaseName)
	assert.ErrorIs(t, lookErr, images.ErrNoSuchImage, "the lock must not pin what was not recorded")
}

func TestSandboxBaseFrom_PublisherMarksTheEntryFullyRecorded(t *testing.T) {
	srv, ref := fakeImageServer(t, map[string]string{"bin/sh": "#!/bin/sh", "etc/os-release": "ID=debian"})
	defer srv.Close()
	repo := newRepo(t)
	_, err := initTrustRoot(t, repo)
	require.NoError(t, err)

	digest := composeInto(t, repo, ref)["base_digest"].(string)

	assert.True(t, testBaseCache(t).FullyRecorded(digest))
}

// Composing bytes that are already cached publishes nothing, so it can never
// assert that the entry's EARLIER pinners recorded themselves.
func TestSandboxBaseFrom_SameBytesAgain_DoesNotMarkAnUnmarkedEntry(t *testing.T) {
	srv, ref := fakeImageServer(t, map[string]string{"bin/sh": "#!/bin/sh", "etc/os-release": "ID=debian"})
	defer srv.Close()
	a, b := newRepo(t), newRepo(t)
	for _, r := range []string{a, b} {
		_, err := initTrustRoot(t, r)
		require.NoError(t, err)
	}
	digest := composeInto(t, a, ref)["base_digest"].(string)
	cache := testBaseCache(t)
	require.NoError(t, cache.ForgetFullyRecorded(digest), "stand in for an entry composed before records")

	require.Equal(t, digest, composeInto(t, b, ref)["base_digest"].(string))

	assert.False(t, cache.FullyRecorded(digest))
}

// A launch whose backfill cannot be written leaves a pin the records do not
// name, so it withdraws the entry's fully-recorded mark, and still launches.
func TestSandboxLaunch_RecordFails_WithdrawsTheMark(t *testing.T) {
	srv, ref := fakeImageServer(t, map[string]string{"bin/sh": "#!/bin/sh", "etc/os-release": "ID=debian"})
	defer srv.Close()
	repo := newRepo(t)
	_, err := initTrustRoot(t, repo)
	require.NoError(t, err)
	digest := composeInto(t, repo, ref)["base_digest"].(string)
	cache := testBaseCache(t)
	require.True(t, cache.FullyRecorded(digest))
	dir := filepath.Join(cache.Root(), "pinners", strings.TrimPrefix(digest, "sha256:"))
	require.NoError(t, os.RemoveAll(dir), "this repository's record is lost")
	require.NoError(t, os.MkdirAll(dir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o750) }) //nolint:gosec // restore for TempDir cleanup

	t.Chdir(repo)
	out, err := runSandbox(okConnect(&fakeSandboxClient{}), "launch",
		"--task", "MGIT-239.3", "--worktree", filepath.Join(t.TempDir(), "wt"))
	require.NoError(t, err, "a failed record must not stop a launch: %s", out)

	assert.False(t, cache.FullyRecorded(digest))
}

// A live daemon's repository is read by prune whether or not it was ever
// recorded, so the daemon record must name that repository's sandbox config
// root: the one the record states, or the repository's own .mgit/sandbox.
func TestDaemonHostRoot_NamesTheSandboxConfigRoot(t *testing.T) {
	assert.Equal(t, "/h", daemonHostRoot(daemonrec.Record{RepoRoot: "/r", HostRoot: "/h"}))
	assert.Equal(t, filepath.Join("/r", ".mgit", "sandbox"), daemonHostRoot(daemonrec.Record{RepoRoot: "/r"}))
}
