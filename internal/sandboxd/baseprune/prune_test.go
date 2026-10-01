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
	live  []string // host roots of live daemons
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

// compose publishes a base and pins it the way compose does: the publisher
// records itself, marks the entry fully recorded, then signs its lock.
func (f *fixture) compose(r repo, content string) (basecache.Entry, string) {
	f.t.Helper()
	entry := f.publish(content)
	require.NoError(f.t, f.cache.RecordPinner(entry.Digest, r.root))
	require.NoError(f.t, f.cache.MarkFullyRecorded(entry.Digest))
	return entry, f.register(r, entry)
}

// register signs entry into the repo's lock and records nothing: a pin made
// by an mgit that wrote no back-reference.
func (f *fixture) register(r repo, entry basecache.Entry) string {
	f.t.Helper()
	ref, err := images.Register(r.root, "base", images.BuildCachedBaseEntry(entry.Digest), r.priv)
	require.NoError(f.t, err)
	return ref
}

// pin registers entry as the repo's base and records the back-reference, as
// a later compose of the same bytes does. It returns the reference.
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
		InUse: func(context.Context) (baseprune.Live, error) {
			return baseprune.Live{Digests: f.inUse, HostRoots: f.live}, f.busy
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
	a, b := f.repo(), f.repo()
	x, refA := f.compose(a, "x")
	y, _ := f.compose(b, "y")
	z, refB := f.compose(b, "z") // b recomposes: y is now pinned by nobody

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
	legacy := f.publish("legacy")
	a, b := f.repo(), f.repo()
	x, _ := f.compose(a, "x")
	y, _ := f.compose(b, "y")
	f.compose(b, "z")

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

// A recorded repository that no longer exists at its recorded path may have
// been deleted, renamed, moved or unmounted; the path cannot say which. Its
// pin is released only on the operator's word, naming that root.
func TestPlan_PinnerRootDeleted_CannotTellUntilReleased(t *testing.T) {
	f := newFixture(t)
	a := f.repo()
	f.compose(a, "x")
	require.NoError(t, os.RemoveAll(filepath.Dir(filepath.Dir(a.root))))

	items, err := baseprune.Plan(t.Context(), f.deps())
	require.NoError(t, err)
	require.Len(t, items, 1)
	assert.Equal(t, baseprune.CannotTell, items[0].Verdict)
	assert.Equal(t, baseprune.PinnerGone, items[0].Pinners[0].State)

	deps := f.deps()
	deps.ReleasedRoots = []string{a.root}
	items, err = baseprune.Plan(t.Context(), deps)
	require.NoError(t, err)
	assert.Equal(t, baseprune.Prunable, items[0].Verdict)
}

// THE REVIEW'S SECOND CASE. A repository renamed in place keeps its parent
// directory and its lock; only its recorded path is gone.
func TestApply_PinnerRenamedInPlace_KeepsTheEntry(t *testing.T) {
	f := newFixture(t)
	a := f.repo()
	x, _ := f.compose(a, "x")
	repoDir := filepath.Dir(filepath.Dir(a.root))
	renamed := repoDir + "-renamed"
	require.NoError(t, os.Rename(repoDir, renamed))

	res, err := baseprune.Apply(t.Context(), f.deps(), nil)
	require.NoError(t, err)
	assert.Empty(t, res.Removed)
	assert.True(t, f.cache.Has(x.Digest))
	pins, err := images.CachedPins(filepath.Join(renamed, ".mgit", "sandbox"))
	require.NoError(t, err)
	assert.True(t, pins[x.Digest], "the renamed repository still pins it")
}

// Releasing a root that exists changes nothing: its lock is still read.
func TestPlan_ReleasedRootThatExists_IsStillRead(t *testing.T) {
	f := newFixture(t)
	a := f.repo()
	x, _ := f.compose(a, "x")
	deps := f.deps()
	deps.ReleasedRoots = []string{a.root}

	items, err := baseprune.Plan(t.Context(), deps)
	require.NoError(t, err)
	assert.Equal(t, baseprune.Pinned, verdicts(items)[x.Digest])
}

// A compose that finds an entry already cached records and signs its pin
// while holding the cache shared; prune takes it exclusively to re-judge and
// remove. So a compose landing between prune's plan and its removal keeps
// the entry instead of signing a lock for a removed one.
func TestApply_ComposeHoldingTheCache_KeepsWhatItPins(t *testing.T) {
	f := newFixture(t)
	b := f.repo()
	y, _ := f.compose(b, "y")
	f.compose(b, "z") // y is prunable
	c := f.repo()
	release, err := f.cache.HoldShared()
	require.NoError(t, err)

	done := make(chan baseprune.Result, 1)
	go func() {
		res, err := baseprune.Apply(context.Background(), f.deps(), nil)
		assert.NoError(t, err)
		done <- res
	}()
	time.Sleep(200 * time.Millisecond) // prune has planned, and waits for the cache
	f.pin(c, y)                        // the compose records and signs, then lets go
	release()

	res := <-done
	assert.Empty(t, res.Removed)
	assert.True(t, f.cache.Has(y.Digest))
}

func TestPlan_AnyLivePinner_KeepsTheEntry(t *testing.T) {
	f := newFixture(t)
	a, b := f.repo(), f.repo()
	x, _ := f.compose(a, "x")
	f.pin(b, x)
	f.compose(b, "z") // b moved on; a still pins x

	items, err := baseprune.Plan(t.Context(), f.deps())
	require.NoError(t, err)
	assert.Equal(t, baseprune.Pinned, verdicts(items)[x.Digest])
}

func TestApply_SandboxRunningOnAnOrphan_NeverRemovesIt(t *testing.T) {
	f := newFixture(t)
	b := f.repo()
	y, _ := f.compose(b, "y")
	f.compose(b, "z")
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
	b := f.repo()
	y, _ := f.compose(b, "y")
	f.compose(b, "z")
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
	b := f.repo()
	f.compose(b, "y")
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
	x, _ := f.compose(f.repo(), "x")

	_, err := baseprune.Apply(t.Context(), f.deps(), []string{x.Digest})
	require.Error(t, err, "the explicit flag is for entries whose pinners are not all recorded")
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
	b := f.repo()
	y, _ := f.compose(b, "y")
	f.compose(b, "z")
	deps := f.deps()
	// The daemons are asked once for the plan and again just before each
	// removal; from that second question on, b pins y again.
	asked := 0
	deps.InUse = func(context.Context) (baseprune.Live, error) {
		asked++
		return baseprune.Live{}, nil
	}
	deps.LockPins = func(root string) (map[string]bool, error) {
		if asked > 1 {
			return map[string]bool{y.Digest: true}, nil
		}
		return images.CachedPins(root)
	}

	res, err := baseprune.Apply(t.Context(), deps, nil)
	require.NoError(t, err)
	assert.Empty(t, res.Removed)
	assert.True(t, f.cache.Has(y.Digest))
}

// THE REVIEW'S CASE. An entry composed before back-references existed gets a
// record when ONE repository launches on it; another repository pins it from
// a build that wrote none. A record proves its own repository pins the entry,
// never that every pinner is recorded, so when the recorded one moves on the
// entry stays: its pinners are still unknown. Refs: MGIT-239
func TestApply_EntryWithAnUnrecordedPinner_IsKept(t *testing.T) {
	f := newFixture(t)
	x := f.publish("x") // no publisher's mark: composed before records
	r, s := f.repo(), f.repo()
	f.pin(r, x) // r's launch backfills its record
	f.register(s, x)
	f.compose(r, "y") // r re-pins

	res, err := baseprune.Apply(t.Context(), f.deps(), nil)
	require.NoError(t, err)
	assert.Empty(t, res.Removed)
	assert.True(t, f.cache.Has(x.Digest))
	assert.Equal(t, baseprune.Unknown, verdicts(res.Items)[x.Digest])
	pins, err := images.CachedPins(s.root)
	require.NoError(t, err)
	assert.True(t, pins[x.Digest], "the unrecorded repository still pins it")
}

// A pin the records do not know about, in a repository whose daemon is
// running, is found by reading that repository's lock too.
func TestPlan_LiveDaemonsRepositoryPinsAnEntry_KeepsIt(t *testing.T) {
	f := newFixture(t)
	r, s := f.repo(), f.repo()
	x, _ := f.compose(r, "x")
	f.register(s, x)
	f.compose(r, "y")
	f.live = []string{s.root}

	items, err := baseprune.Plan(t.Context(), f.deps())
	require.NoError(t, err)
	assert.Equal(t, baseprune.Pinned, verdicts(items)[x.Digest])
}

// A recorded repository that is missing together with the directory that
// held it is a moved tree or an unmounted volume as likely as a deletion.
func TestPlan_PinnerRootAndItsParentMissing_CannotTell(t *testing.T) {
	f := newFixture(t)
	parent := filepath.Join(t.TempDir(), "volume")
	root := filepath.Join(parent, "repo", ".mgit", "sandbox")
	require.NoError(t, os.MkdirAll(root, 0o750))
	priv, err := images.GenerateTrustRoot(t.Context(), root, noopAuditor{})
	require.NoError(t, err)
	r := repo{root: root, priv: priv}
	x, _ := f.compose(r, "x")
	require.NoError(t, os.RemoveAll(parent))

	items, err := baseprune.Plan(t.Context(), f.deps())
	require.NoError(t, err)
	assert.Equal(t, baseprune.CannotTell, verdicts(items)[x.Digest])
	assert.Equal(t, baseprune.PinnerGone, items[0].Pinners[0].State)
}

func TestApply_EntryWithAnUnrecordedPinnerNamed_IsRemoved(t *testing.T) {
	f := newFixture(t)
	x := f.publish("x")
	r := f.repo()
	f.pin(r, x)
	f.compose(r, "y")

	res, err := baseprune.Apply(t.Context(), f.deps(), []string{x.Digest})
	require.NoError(t, err)
	require.Len(t, res.Removed, 1, "naming is the consent for an entry whose pinners are not all known")
	assert.Equal(t, x.Digest, res.Removed[0].Digest)
}
