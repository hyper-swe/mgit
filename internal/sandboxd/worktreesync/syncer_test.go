package worktreesync

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/sandboxd/staging"
)

// fixture is one sandbox's host-side layout: a live worktree, a private
// store, a state dir, and the tree the guest sees — staged exactly as a
// launch would stage it, with the delivery baseline recorded.
type fixture struct {
	worktree, store, stateDir, guestTree string
}

func newFixture(t *testing.T, files map[string]string) fixture {
	t.Helper()
	f := fixture{
		worktree: t.TempDir(),
		store:    t.TempDir(),
		stateDir: t.TempDir(),
	}
	f.guestTree = filepath.Join(f.stateDir, "worktree-staging")
	writeTree(t, f.worktree, files)
	require.NoError(t, os.WriteFile(filepath.Join(f.store, "HEAD"), []byte("ref: refs/heads/task"), 0o600))
	// Stage as a launch does, then record what was delivered.
	require.NoError(t, staging.Build(f.worktree, f.store, f.guestTree))
	require.NoError(t, RecordDelivery(f.guestTree, f.stateDir))
	return f
}

func (f fixture) sync(force bool) (Result, error) {
	return Sync(Request{
		WorktreePath: f.worktree, PrivateStorePath: f.store,
		StateDir: f.stateDir, GuestTree: f.guestTree, Force: force,
	})
}

// TestSync_HostEditBetweenExecs_ReachesTheGuest is the HyperSwe repro at the
// unit layer: a host file written after launch is visible in the guest's tree.
// Refs: MGIT-71, hyper-swe/swe PR #68
func TestSync_HostEditBetweenExecs_ReachesTheGuest(t *testing.T) {
	f := newFixture(t, map[string]string{"app.go": "V1"})
	require.Equal(t, "V1", readFile(t, filepath.Join(f.guestTree, "app.go")))

	writeTree(t, f.worktree, map[string]string{"app.go": "V2"})
	res, err := f.sync(false)

	require.NoError(t, err)
	assert.Equal(t, []string{"app.go"}, res.Updated)
	assert.Equal(t, "V2", readFile(t, filepath.Join(f.guestTree, "app.go")),
		"the guest must see the NEW content, not merely lose the old")
}

// TestSync_UnchangedHost_IsSkipped verifies the affordability property that
// lets this run before every exec.
func TestSync_UnchangedHost_IsSkipped(t *testing.T) {
	f := newFixture(t, map[string]string{"app.go": "V1"})

	res, err := f.sync(false)

	require.NoError(t, err)
	assert.True(t, res.Skipped)
	assert.False(t, res.Changed())
}

// TestSync_GuestWorkIsNotSilentlyDestroyed is the collision policy end to end:
// a host edit to a path the guest also edited is REFUSED, nothing is applied,
// and the refusal names the path. Refs: MGIT-71
func TestSync_GuestWorkIsNotSilentlyDestroyed(t *testing.T) {
	f := newFixture(t, map[string]string{"app.go": "V1"})
	writeTree(t, f.guestTree, map[string]string{"app.go": "GUEST-WIP"})
	writeTree(t, f.worktree, map[string]string{"app.go": "V2"})

	_, err := f.sync(false)

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrConflict)
	assert.Contains(t, err.Error(), "app.go")
	assert.Contains(t, err.Error(), "--force", "the refusal must name a remedy")
	assert.Equal(t, "GUEST-WIP", readFile(t, filepath.Join(f.guestTree, "app.go")),
		"nothing is applied when the sync is refused")
}

// TestSync_Force_OverwritesAndReportsEveryOverriddenPath verifies --force is
// available but never silent: each destroyed path is reported so it lands in
// the audit record.
func TestSync_Force_OverwritesAndReportsEveryOverriddenPath(t *testing.T) {
	f := newFixture(t, map[string]string{"app.go": "V1"})
	writeTree(t, f.guestTree, map[string]string{"app.go": "GUEST-WIP"})
	writeTree(t, f.worktree, map[string]string{"app.go": "V2"})

	res, err := f.sync(true)

	require.NoError(t, err)
	assert.Equal(t, []string{"app.go"}, res.Overridden)
	assert.Equal(t, "V2", readFile(t, filepath.Join(f.guestTree, "app.go")))
}

// TestSync_GuestCreatedArtifactsSurvive is what keeps the agent loop viable:
// a build cache the guest produced is untouched by any number of syncs.
// Refs: MGIT-71, MGIT-73
func TestSync_GuestCreatedArtifactsSurvive(t *testing.T) {
	f := newFixture(t, map[string]string{"app.go": "V1"})
	writeTree(t, f.guestTree, map[string]string{"node_modules/dep/index.js": "CACHED"})

	writeTree(t, f.worktree, map[string]string{"app.go": "V2"})
	_, err := f.sync(false)
	require.NoError(t, err)
	writeTree(t, f.worktree, map[string]string{"app.go": "V3"})
	_, err = f.sync(false)
	require.NoError(t, err)

	assert.Equal(t, "CACHED", readFile(t, filepath.Join(f.guestTree, "node_modules", "dep", "index.js")))
	assert.Equal(t, "V3", readFile(t, filepath.Join(f.guestTree, "app.go")))
}

// TestSync_HostDeletion_Propagates verifies a file removed on the host goes
// from the guest too, when the guest never touched it.
func TestSync_HostDeletion_Propagates(t *testing.T) {
	f := newFixture(t, map[string]string{"app.go": "V1", "old.go": "REMOVE ME"})
	require.NoError(t, os.Remove(filepath.Join(f.worktree, "old.go")))

	res, err := f.sync(false)

	require.NoError(t, err)
	assert.Equal(t, []string{"old.go"}, res.Deleted)
	_, statErr := os.Stat(filepath.Join(f.guestTree, "old.go"))
	assert.True(t, os.IsNotExist(statErr))
}

// TestSync_PrivateStoreIsUntouched verifies the guest's own store — where its
// commits live until land carries them out — survives a sync. Refs: SEC-03
func TestSync_PrivateStoreIsUntouched(t *testing.T) {
	f := newFixture(t, map[string]string{"app.go": "V1"})
	guestCommit := filepath.Join(f.guestTree, staging.GuestStoreName, "refs", "heads", "task")
	require.NoError(t, os.MkdirAll(filepath.Dir(guestCommit), 0o750))
	require.NoError(t, os.WriteFile(guestCommit, []byte("deadbeef"), 0o600))

	writeTree(t, f.worktree, map[string]string{"app.go": "V2"})
	_, err := f.sync(false)

	require.NoError(t, err)
	assert.Equal(t, "deadbeef", readFile(t, guestCommit), "the guest's commits survive a sync")
}

// TestSync_EscapingSymlink_IsRefusedLikeALaunch verifies a sync cannot deliver
// what a launch would have rejected: the SEC-03 invariants are re-enforced on
// every propagation, not just at boot. Refs: SEC-03, MGIT-71
func TestSync_EscapingSymlink_IsRefusedLikeALaunch(t *testing.T) {
	f := newFixture(t, map[string]string{"app.go": "V1"})
	outside := filepath.Join(t.TempDir(), "host-secret")
	require.NoError(t, os.WriteFile(outside, []byte("host secret"), 0o600))
	require.NoError(t, os.Symlink(outside, filepath.Join(f.worktree, "escape")))

	_, err := f.sync(false)

	require.Error(t, err)
	assert.True(t, errors.Is(err, staging.ErrSymlinkEscape),
		"a sync must fail closed exactly as a launch does, got %v", err)
	_, statErr := os.Stat(filepath.Join(f.guestTree, "escape"))
	assert.True(t, os.IsNotExist(statErr), "nothing escaping reached the guest")
}

// TestSync_InWorktreeStore_IsNeverDelivered verifies the other SEC-03
// invariant holds on propagation: a .git that appears in the host worktree
// after launch still never reaches the guest.
func TestSync_InWorktreeStore_IsNeverDelivered(t *testing.T) {
	f := newFixture(t, map[string]string{"app.go": "V1"})
	writeTree(t, f.worktree, map[string]string{".git/config": "[core]", "vendor/dep/.git/config": "[core]"})

	_, err := f.sync(false)

	require.NoError(t, err)
	for _, leaked := range []string{".git/config", "vendor/dep/.git/config"} {
		_, statErr := os.Stat(filepath.Join(f.guestTree, leaked))
		assert.True(t, os.IsNotExist(statErr), "%s must not reach the guest", leaked)
	}
}

// TestSync_FailedApply_LeavesTheBaselineIntact verifies a sync that cannot
// complete does not advance the delivery baseline, so the next attempt
// re-derives the same work instead of believing it already happened.
func TestSync_FailedApply_LeavesTheBaselineIntact(t *testing.T) {
	f := newFixture(t, map[string]string{"app.go": "V1"})
	before, err := LoadManifest(f.stateDir)
	require.NoError(t, err)

	// A guest-side conflict blocks the sync.
	writeTree(t, f.guestTree, map[string]string{"app.go": "GUEST-WIP"})
	writeTree(t, f.worktree, map[string]string{"app.go": "V2"})
	_, syncErr := f.sync(false)
	require.Error(t, syncErr)

	after, err := LoadManifest(f.stateDir)
	require.NoError(t, err)
	assert.Equal(t, before, after, "a refused sync must not move the baseline")
}

// TestSync_ManifestRoundTrips verifies the baseline survives being written and
// read, since every classification depends on it.
func TestSync_ManifestRoundTrips(t *testing.T) {
	dir := t.TempDir()
	want := Manifest{"a.go": {Hash: "abc", Mode: 0o644}, "run.sh": {Hash: "def", Mode: 0o755}}

	require.NoError(t, SaveManifest(dir, want))
	got, err := LoadManifest(dir)

	require.NoError(t, err)
	assert.Equal(t, want, got)
}

// TestLoadManifest_Absent_IsEmpty verifies a sandbox launched before this
// existed degrades safely rather than erroring.
func TestLoadManifest_Absent_IsEmpty(t *testing.T) {
	got, err := LoadManifest(t.TempDir())

	require.NoError(t, err)
	assert.Empty(t, got)
}
