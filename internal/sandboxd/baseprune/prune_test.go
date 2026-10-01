package baseprune_test

import (
	"context"
	"crypto/ed25519"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/sandboxd/basecache"
	"github.com/hyper-swe/mgit/internal/sandboxd/baseprune"
	"github.com/hyper-swe/mgit/internal/sandboxd/images"
)

type noopAuditor struct{}

func (noopAuditor) RecordTrustRootChange(context.Context, string) error { return nil }

// fixture is one machine: a shared cache and the repositories pinning into it.
type fixture struct {
	t     *testing.T
	cache *basecache.Cache
	inUse map[string]bool
	busy  error
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	cache, err := basecache.New(t.TempDir())
	require.NoError(t, err)
	return &fixture{t: t, cache: cache, inUse: map[string]bool{}}
}

// publish composes a one-file base into the cache.
func (f *fixture) publish(content string) basecache.Entry {
	f.t.Helper()
	staging, err := f.cache.Stage()
	require.NoError(f.t, err)
	require.NoError(f.t, os.WriteFile(filepath.Join(staging, "os-release"), []byte(content), 0o600))
	entry, err := f.cache.Commit(staging, images.TreeDigest)
	require.NoError(f.t, err)
	return entry
}

// repo is a repository's sandbox config root with its signing key.
type repo struct {
	root string
	priv ed25519.PrivateKey
}

func (f *fixture) repo() repo {
	f.t.Helper()
	root := filepath.Join(f.t.TempDir(), ".mgit", "sandbox")
	require.NoError(f.t, os.MkdirAll(root, 0o750))
	priv, err := images.GenerateTrustRoot(f.t.Context(), root, noopAuditor{})
	require.NoError(f.t, err)
	return repo{root: root, priv: priv}
}

// pin registers entry as the repo's base and records the back-reference,
// as a compose does. It returns the digest-pinned reference.
func (f *fixture) pin(r repo, entry basecache.Entry) string {
	f.t.Helper()
	ref, err := images.Register(r.root, "base", images.BuildCachedBaseEntry(entry.Digest), r.priv)
	require.NoError(f.t, err)
	require.NoError(f.t, f.cache.RecordPinner(entry.Digest, r.root))
	return ref
}

func (f *fixture) deps() baseprune.Deps {
	return baseprune.Deps{
		Cache:    f.cache,
		LockPins: images.CachedPins,
		InUse: func(context.Context) (map[string]bool, error) {
			return f.inUse, f.busy
		},
	}
}

func verdicts(items []baseprune.Item) map[string]baseprune.Verdict {
	out := map[string]baseprune.Verdict{}
	for _, it := range items {
		out[it.Digest] = it.Verdict
	}
	return out
}

// THE SCENARIO THE TICKET NAMES. Two repositories pin two entries; one of
// them re-pins elsewhere; prune removes only the orphan, and the other
// repository still launches on what it pinned. Refs: MGIT-239
func TestApply_OneRepoRepins_RemovesOnlyTheOrphanAndTheOtherStillLaunches(t *testing.T) {
	f := newFixture(t)
	x, y, z := f.publish("x"), f.publish("y"), f.publish("z")
	a, b := f.repo(), f.repo()
	refA := f.pin(a, x)
	f.pin(b, y)
	refB := f.pin(b, z) // b recomposes: y is now pinned by nobody

	res, err := baseprune.Apply(t.Context(), f.deps(), nil)
	require.NoError(t, err)

	require.Len(t, res.Removed, 1)
	assert.Equal(t, y.Digest, res.Removed[0].Digest)
	assert.Positive(t, res.Freed)
	assert.False(t, f.cache.Has(y.Digest))
	assert.True(t, f.cache.Has(x.Digest))
	assert.True(t, f.cache.Has(z.Digest))
	for _, rr := range []struct {
		root, ref string
	}{{a.root, refA}, {b.root, refB}} {
		store, err := images.NewStoreWithBaseCache(rr.root, time.Now, f.cache)
		require.NoError(t, err)
		_, err = store.Resolve(rr.ref)
		assert.NoError(t, err, "a repository whose pin survives must still resolve its base")
	}
}

func TestPlan_DryRun_ChangesNothingAndNamesEachVerdict(t *testing.T) {
	f := newFixture(t)
	x, y, legacy := f.publish("x"), f.publish("y"), f.publish("legacy")
	a, b := f.repo(), f.repo()
	f.pin(a, x)
	f.pin(b, y)
	f.pin(b, f.publish("z"))

	items, err := baseprune.Plan(t.Context(), f.deps())
	require.NoError(t, err)

	got := verdicts(items)
	assert.Equal(t, baseprune.Pinned, got[x.Digest])
	assert.Equal(t, baseprune.Prunable, got[y.Digest])
	assert.Equal(t, baseprune.Unknown, got[legacy.Digest])
	for _, e := range []basecache.Entry{x, y, legacy} {
		assert.True(t, f.cache.Has(e.Digest), "a plan removes nothing")
	}
	for _, it := range items {
		assert.Positive(t, it.Bytes, "every entry is listed with its size")
		if it.Digest == y.Digest {
			require.Len(t, it.Pinners, 1)
			assert.Equal(t, baseprune.PinnerRepinned, it.Pinners[0].State)
		}
	}
}

func TestPlan_PinnerRootDeleted_EntryIsPrunable(t *testing.T) {
	f := newFixture(t)
	x := f.publish("x")
	a := f.repo()
	f.pin(a, x)
	require.NoError(t, os.RemoveAll(filepath.Dir(filepath.Dir(a.root))))

	items, err := baseprune.Plan(t.Context(), f.deps())
	require.NoError(t, err)
	require.Len(t, items, 1)
	assert.Equal(t, baseprune.Prunable, items[0].Verdict)
	assert.Equal(t, baseprune.PinnerGone, items[0].Pinners[0].State)
}

func TestPlan_AnyLivePinner_KeepsTheEntry(t *testing.T) {
	f := newFixture(t)
	x := f.publish("x")
	a, b := f.repo(), f.repo()
	f.pin(a, x)
	f.pin(b, x)
	f.pin(b, f.publish("z")) // b moved on; a still pins x

	items, err := baseprune.Plan(t.Context(), f.deps())
	require.NoError(t, err)
	assert.Equal(t, baseprune.Pinned, verdicts(items)[x.Digest])
}

func TestApply_SandboxRunningOnAnOrphan_NeverRemovesIt(t *testing.T) {
	f := newFixture(t)
	y := f.publish("y")
	b := f.repo()
	f.pin(b, y)
	f.pin(b, f.publish("z"))
	f.inUse[y.Digest] = true

	res, err := baseprune.Apply(t.Context(), f.deps(), nil)
	require.NoError(t, err)
	assert.Empty(t, res.Removed)
	assert.True(t, f.cache.Has(y.Digest))
	assert.Equal(t, baseprune.InUse, verdicts(res.Items)[y.Digest])
}

// Whether a sandbox runs on an entry is a question only the daemons can
// answer. When one cannot be asked, silence is not "nothing runs": nothing is
// removed, and the plan says why.
func TestApply_DaemonCannotBeAsked_RemovesNothing(t *testing.T) {
	f := newFixture(t)
	y := f.publish("y")
	b := f.repo()
	f.pin(b, y)
	f.pin(b, f.publish("z"))
	f.busy = errors.New("daemon for /some/repo did not answer")

	res, err := baseprune.Apply(t.Context(), f.deps(), nil)
	require.NoError(t, err)
	assert.Empty(t, res.Removed)
	assert.True(t, f.cache.Has(y.Digest))
	assert.Equal(t, baseprune.CannotTell, verdicts(res.Items)[y.Digest])
	assert.Contains(t, res.Items[0].Reason+res.Items[1].Reason, "did not answer")
}

func TestPlan_PinnerLockUnreadable_KeepsTheEntry(t *testing.T) {
	f := newFixture(t)
	y := f.publish("y")
	b := f.repo()
	f.pin(b, y)
	deps := f.deps()
	deps.LockPins = func(string) (map[string]bool, error) { return nil, errors.New("permission denied") }

	items, err := baseprune.Plan(t.Context(), deps)
	require.NoError(t, err)
	assert.Equal(t, baseprune.CannotTell, items[0].Verdict)
}

func TestApply_UnknownPinner_KeptByDefault(t *testing.T) {
	f := newFixture(t)
	legacy := f.publish("legacy")

	res, err := baseprune.Apply(t.Context(), f.deps(), nil)
	require.NoError(t, err)
	assert.Empty(t, res.Removed)
	assert.True(t, f.cache.Has(legacy.Digest))
}

func TestApply_UnknownPinnerNamedExplicitly_IsRemoved(t *testing.T) {
	f := newFixture(t)
	legacy, other := f.publish("legacy"), f.publish("other")

	res, err := baseprune.Apply(t.Context(), f.deps(), []string{legacy.Digest})
	require.NoError(t, err)
	require.Len(t, res.Removed, 1)
	assert.Equal(t, legacy.Digest, res.Removed[0].Digest)
	assert.True(t, f.cache.Has(other.Digest), "only the named entry is removed")
}

func TestApply_UnknownPinnerNamedButInUse_IsKept(t *testing.T) {
	f := newFixture(t)
	legacy := f.publish("legacy")
	f.inUse[legacy.Digest] = true

	res, err := baseprune.Apply(t.Context(), f.deps(), []string{legacy.Digest})
	require.NoError(t, err)
	assert.Empty(t, res.Removed)
	assert.True(t, f.cache.Has(legacy.Digest))
}

func TestApply_NamedDigestThatIsNotUnknown_Refused(t *testing.T) {
	f := newFixture(t)
	x := f.publish("x")
	f.pin(f.repo(), x)

	_, err := baseprune.Apply(t.Context(), f.deps(), []string{x.Digest})
	require.Error(t, err, "the explicit flag is for entries with no recorded pinner only")
	assert.True(t, f.cache.Has(x.Digest))
}

func TestApply_NamedDigestNotInTheCache_Refused(t *testing.T) {
	f := newFixture(t)
	_, err := baseprune.Apply(t.Context(), f.deps(),
		[]string{"sha256:0000000000000000000000000000000000000000000000000000000000000000"})
	require.Error(t, err)
}

// A pinner that pins the entry again between the plan and the removal keeps
// it: the verdict is taken again for each entry immediately before it goes.
func TestApply_PinnedAgainAfterThePlan_IsKept(t *testing.T) {
	f := newFixture(t)
	y := f.publish("y")
	b := f.repo()
	f.pin(b, y)
	f.pin(b, f.publish("z"))
	deps := f.deps()
	calls := 0
	deps.LockPins = func(root string) (map[string]bool, error) {
		calls++
		if calls > 1 { // the plan has been taken; b re-pins y
			return map[string]bool{y.Digest: true}, nil
		}
		return images.CachedPins(root)
	}

	res, err := baseprune.Apply(t.Context(), deps, nil)
	require.NoError(t, err)
	assert.Empty(t, res.Removed)
	assert.True(t, f.cache.Has(y.Digest))
}
