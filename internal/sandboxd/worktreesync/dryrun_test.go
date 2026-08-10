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

// dryRun runs a classification-only sync over the fixture.
func (f fixture) dryRun(force bool) (Result, error) {
	return Sync(Request{
		WorktreePath: f.worktree, PrivateStorePath: f.store,
		StateDir: f.stateDir, GuestTree: f.guestTree, Force: force, DryRun: true,
	})
}

// TestSync_DryRun_ReportsUpdatesWithoutTouchingTheGuest is the capability
// MGIT-76 adds: the classification is obtainable without writing anything into
// the guest — and without running a command there to provoke it.
// Refs: MGIT-76, ADR-011
func TestSync_DryRun_ReportsUpdatesWithoutTouchingTheGuest(t *testing.T) {
	f := newFixture(t, map[string]string{"app.go": "V1", "old.go": "REMOVE ME"})
	writeTree(t, f.worktree, map[string]string{"app.go": "V2"})
	require.NoError(t, os.Remove(filepath.Join(f.worktree, "old.go")))

	res, err := f.dryRun(false)

	require.NoError(t, err)
	assert.True(t, res.DryRun)
	assert.Equal(t, []string{"app.go"}, res.Updated)
	assert.Equal(t, []string{"old.go"}, res.Deleted)
	assert.False(t, res.Blocked, "no guest-side change means nothing blocks the sync")
	assert.Equal(t, "V1", readFile(t, filepath.Join(f.guestTree, "app.go")),
		"a dry run must not deliver anything")
	assert.FileExists(t, filepath.Join(f.guestTree, "old.go"),
		"a dry run must not delete anything")
}

// TestSync_DryRun_ReportsConflictsAsDataNotRefusal is the report HyperSwe
// cannot obtain today: which paths diverged, WITHOUT attempting work and being
// refused. A dry run is a query, so conflicts come back as a classification
// rather than an error. Refs: MGIT-76
func TestSync_DryRun_ReportsConflictsAsDataNotRefusal(t *testing.T) {
	f := newFixture(t, map[string]string{"app.go": "V1", "doc.md": "D1"})
	writeTree(t, f.guestTree, map[string]string{"app.go": "GUEST-WIP"})
	writeTree(t, f.worktree, map[string]string{"app.go": "V2", "doc.md": "D2"})

	res, err := f.dryRun(false)

	require.NoError(t, err, "a dry run reports; it does not refuse")
	assert.True(t, res.Blocked, "the report must say the real sync would be refused")
	require.Len(t, res.Conflicts, 1)
	assert.Equal(t, "app.go", res.Conflicts[0].Path)
	assert.Equal(t, ReasonModifiedInGuest, res.Conflicts[0].Reason)
	assert.Equal(t, []string{"doc.md"}, res.Updated,
		"the paths that WOULD move are reported alongside the conflicts")
	assert.Equal(t, "GUEST-WIP", readFile(t, filepath.Join(f.guestTree, "app.go")))
	assert.Equal(t, "D1", readFile(t, filepath.Join(f.guestTree, "doc.md")),
		"a blocked plan delivers nothing, not even its unblocked paths")
}

// TestSync_DryRun_GuestCreatedCollision_IsClassifiedByReason verifies the
// third collision class surfaces with its own reason, so a caller can tell
// "the guest edited what I delivered" from "the guest made its own file of
// that name". Refs: MGIT-71, MGIT-76
func TestSync_DryRun_GuestCreatedCollision_IsClassifiedByReason(t *testing.T) {
	f := newFixture(t, map[string]string{"app.go": "V1"})
	writeTree(t, f.guestTree, map[string]string{"generated.go": "GUEST OUTPUT"})
	writeTree(t, f.worktree, map[string]string{"generated.go": "HOST NOW HAS ONE TOO"})

	res, err := f.dryRun(false)

	require.NoError(t, err)
	require.Len(t, res.Conflicts, 1)
	assert.Equal(t, "generated.go", res.Conflicts[0].Path)
	assert.Equal(t, ReasonCreatedInGuest, res.Conflicts[0].Reason)
}

// TestSync_DryRun_UnchangedHost_IsSkipped verifies the cheap no-op answer is
// available as a query too: an unchanged worktree reports nothing to do rather
// than phantom work. Refs: MGIT-76
func TestSync_DryRun_UnchangedHost_IsSkipped(t *testing.T) {
	f := newFixture(t, map[string]string{"app.go": "V1"})

	res, err := f.dryRun(false)

	require.NoError(t, err)
	assert.True(t, res.Skipped)
	assert.True(t, res.DryRun)
	assert.False(t, res.Changed())
	assert.Empty(t, res.Conflicts)
}

// TestSync_DryRun_DoesNotAdvanceTheBaseline verifies a query leaves the
// delivery record alone — otherwise a dry run would make the next REAL sync
// believe the work had already been delivered. Refs: MGIT-76
func TestSync_DryRun_DoesNotAdvanceTheBaseline(t *testing.T) {
	f := newFixture(t, map[string]string{"app.go": "V1"})
	before, err := LoadManifest(f.stateDir)
	require.NoError(t, err)

	writeTree(t, f.worktree, map[string]string{"app.go": "V2"})
	_, err = f.dryRun(false)
	require.NoError(t, err)

	after, err := LoadManifest(f.stateDir)
	require.NoError(t, err)
	assert.Equal(t, before, after, "a dry run must not move the baseline")

	// And the real sync that follows still does the work.
	res, err := f.sync(false)
	require.NoError(t, err)
	assert.Equal(t, []string{"app.go"}, res.Updated)
	assert.Equal(t, "V2", readFile(t, filepath.Join(f.guestTree, "app.go")))
}

// TestSync_DryRun_Force_ReportsWhatWouldBeDestroyed verifies --dry-run
// --force answers the question that actually matters before forcing: exactly
// which un-landed guest paths would be destroyed. Refs: MGIT-76
func TestSync_DryRun_Force_ReportsWhatWouldBeDestroyed(t *testing.T) {
	f := newFixture(t, map[string]string{"app.go": "V1"})
	writeTree(t, f.guestTree, map[string]string{"app.go": "GUEST-WIP"})
	writeTree(t, f.worktree, map[string]string{"app.go": "V2"})

	res, err := f.dryRun(true)

	require.NoError(t, err)
	assert.False(t, res.Blocked, "a forced plan is not blocked")
	assert.Equal(t, []string{"app.go"}, res.Overridden)
	assert.Equal(t, "GUEST-WIP", readFile(t, filepath.Join(f.guestTree, "app.go")),
		"a forced DRY RUN still destroys nothing")
}

// TestSync_DryRun_EscapingSymlink_StillFailsClosed verifies the query runs the
// same staging build the real sync does, so it can never report that a sync
// would succeed when a launch would have refused it. Refs: SEC-03, MGIT-76
func TestSync_DryRun_EscapingSymlink_StillFailsClosed(t *testing.T) {
	f := newFixture(t, map[string]string{"app.go": "V1"})
	outside := filepath.Join(t.TempDir(), "host-secret")
	require.NoError(t, os.WriteFile(outside, []byte("host secret"), 0o600))
	require.NoError(t, os.Symlink(outside, filepath.Join(f.worktree, "escape")))

	_, err := f.dryRun(false)

	require.Error(t, err)
	assert.True(t, errors.Is(err, staging.ErrSymlinkEscape),
		"a dry run must fail closed exactly as a launch does, got %v", err)
}

// TestSync_DryRun_LeavesNoCandidateTreeBehind verifies the classification's
// scratch tree is reclaimed, so repeated polling cannot fill the state dir.
func TestSync_DryRun_LeavesNoCandidateTreeBehind(t *testing.T) {
	f := newFixture(t, map[string]string{"app.go": "V1"})
	writeTree(t, f.worktree, map[string]string{"app.go": "V2"})

	_, err := f.dryRun(false)

	require.NoError(t, err)
	_, statErr := os.Stat(filepath.Join(f.stateDir, stagedSubdir))
	assert.True(t, os.IsNotExist(statErr), "the candidate tree must be cleaned up")
}

// TestLoadManifest_Corrupt_FailsLoudly verifies a damaged delivery baseline is
// an error rather than an empty manifest.
//
// This matters more than it looks: an empty baseline makes every host path look
// NEW, which silently reclassifies the whole tree. Failing closed keeps a
// corrupt record from quietly changing what a sync decides. Refs: MGIT-71
func TestLoadManifest_Corrupt_FailsLoudly(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, manifestName), []byte("{not json"), 0o600))

	_, err := LoadManifest(dir)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "delivery manifest")
}

// TestSync_CorruptBaseline_RefusesRatherThanReclassifying verifies the refusal
// reaches the caller instead of being absorbed into a plan derived from an
// empty baseline — for a dry run too, since a report built on a lost baseline
// would be confidently wrong. Refs: MGIT-71, MGIT-76
func TestSync_CorruptBaseline_RefusesRatherThanReclassifying(t *testing.T) {
	f := newFixture(t, map[string]string{"app.go": "V1"})
	require.NoError(t, os.WriteFile(filepath.Join(f.stateDir, manifestName), []byte("{"), 0o600))
	writeTree(t, f.worktree, map[string]string{"app.go": "V2"})

	for name, dry := range map[string]bool{"real": false, "dry_run": true} {
		t.Run(name, func(t *testing.T) {
			_, err := Sync(Request{
				WorktreePath: f.worktree, PrivateStorePath: f.store,
				StateDir: f.stateDir, GuestTree: f.guestTree, DryRun: dry,
			})

			require.Error(t, err)
			assert.Equal(t, "V1", readFile(t, filepath.Join(f.guestTree, "app.go")),
				"nothing is delivered when the baseline cannot be read")
		})
	}
}

// TestSortedPaths_IsStableAndDoesNotAliasTheInput verifies the audit helper
// orders its output and leaves the caller's slice alone — a log line that
// reordered the caller's data would be a surprising side effect.
func TestSortedPaths_IsStableAndDoesNotAliasTheInput(t *testing.T) {
	in := []string{"z.go", "a.go", "m.go"}

	got := SortedPaths(in)

	assert.Equal(t, []string{"a.go", "m.go", "z.go"}, got)
	assert.Equal(t, []string{"z.go", "a.go", "m.go"}, in, "the input must not be reordered")
	assert.Empty(t, SortedPaths(nil))
}

// TestBuildManifest_UnreadableEntry_FailsClosed verifies a tree that cannot be
// hashed refuses the sync rather than reporting a short manifest, which would
// look like the host had deleted the files it could not read.
func TestBuildManifest_UnreadableEntry_FailsClosed(t *testing.T) {
	root := t.TempDir()
	blocked := filepath.Join(root, "blocked")
	require.NoError(t, os.MkdirAll(blocked, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(blocked, "secret.go"), []byte("x"), 0o600))
	require.NoError(t, os.Chmod(blocked, 0o000))
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o750) }) //nolint:gosec // G302: a DIRECTORY mode, not a file's — a directory needs its execute bit to be traversable, and these modes deliberately inject the fault under test

	_, err := BuildManifest(root)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "read tree")
}

// TestApply_UnsafePlanPath_IsRefused verifies the defense-in-depth check on
// Apply itself. Plans are host-derived today, so nothing should ever reach it
// — which is exactly why it needs its own test rather than relying on a caller
// that currently happens to be well-behaved. Refs: SEC-03
func TestApply_UnsafePlanPath_IsRefused(t *testing.T) {
	staged, guest := t.TempDir(), t.TempDir()
	for name, rel := range map[string]string{
		"absolute":  filepath.Join(t.TempDir(), "outside.go"),
		"traversal": filepath.Join("..", "outside.go"),
	} {
		t.Run(name, func(t *testing.T) {
			err := Apply(staged, guest, Plan{Update: []string{rel}})

			require.Error(t, err)
			assert.ErrorIs(t, err, ErrUnsafePath)
		})
	}
	t.Run("delete", func(t *testing.T) {
		err := Apply(staged, guest, Plan{Delete: []string{filepath.Join("..", "outside.go")}})

		require.Error(t, err)
		assert.ErrorIs(t, err, ErrUnsafePath)
	})
}

// TestSync_ApplyFails_LeavesTheBaselineIntact verifies the manifest advances
// only after a SUCCESSFUL apply. If a partial write moved the baseline, the
// next sync would believe the undelivered paths were already in the guest —
// stale code, reported as up to date. Refs: MGIT-71, MGIT-76
func TestSync_ApplyFails_LeavesTheBaselineIntact(t *testing.T) {
	f := newFixture(t, map[string]string{"app.go": "V1"})
	before, err := LoadManifest(f.stateDir)
	require.NoError(t, err)

	// A NEW host path: delivering it needs a directory entry the guest tree
	// will not allow, so the apply fails partway through.
	writeTree(t, f.worktree, map[string]string{"added.go": "NEW"})
	require.NoError(t, os.Chmod(f.guestTree, 0o500))       //nolint:gosec // G302: a DIRECTORY mode, not a file's — a directory needs its execute bit to be traversable, and these modes deliberately inject the fault under test
	t.Cleanup(func() { _ = os.Chmod(f.guestTree, 0o750) }) //nolint:gosec // G302: a DIRECTORY mode, not a file's — a directory needs its execute bit to be traversable, and these modes deliberately inject the fault under test

	_, syncErr := f.sync(false)

	require.Error(t, syncErr)
	after, err := LoadManifest(f.stateDir)
	require.NoError(t, err)
	assert.Equal(t, before, after, "a failed apply must not move the baseline")
}

// TestBuildManifest_Symlink_IsHashedByTargetNotContent verifies a link and the
// file it points at stay distinguishable, and that building a manifest never
// follows a link out of the tree. Refs: SEC-03
func TestBuildManifest_Symlink_IsHashedByTargetNotContent(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "real.go"), []byte("CONTENT"), 0o600))
	require.NoError(t, os.Symlink("real.go", filepath.Join(root, "link.go")))

	got, err := BuildManifest(root)

	require.NoError(t, err)
	require.Contains(t, got, "link.go")
	assert.NotEqual(t, got["real.go"].Hash, got["link.go"].Hash,
		"a symlink must not hash as the content it points at")
	assert.NotZero(t, got["link.go"].Mode&os.ModeSymlink, "the link is recorded as a link")
}
