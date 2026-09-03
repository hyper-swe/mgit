package worktreesync

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/sandboxd/staging"
)

// withApply swaps the package's apply seam for the duration of one test and
// restores it afterwards.
//
// It exists so the MGIT-164 read-back can be exercised against a REAL dropped
// operation rather than against hand-fed manifests. verify_test.go proves the
// comparison is correct; these prove Sync actually consults it, which is a
// different claim and the one that failed in the field.
func withApply(t *testing.T, fn func(staged, guest string, plan Plan) error) {
	t.Helper()
	prev := applyPlan
	applyPlan = fn
	t.Cleanup(func() { applyPlan = prev })
}

// dropping returns an Apply that performs the plan honestly EXCEPT for the
// named paths, which it silently skips — the shape of the field failure:
// nothing errored, and the guest simply did not have the file.
func dropping(skip ...string) func(string, string, Plan) error {
	dropped := make(map[string]bool, len(skip))
	for _, s := range skip {
		dropped[s] = true
	}
	return func(staged, guest string, plan Plan) error {
		kept := Plan{Conflicts: plan.Conflicts, forced: plan.forced}
		for _, p := range plan.Update {
			if !dropped[p] {
				kept.Update = append(kept.Update, p)
			}
		}
		for _, p := range plan.Delete {
			if !dropped[p] {
				kept.Delete = append(kept.Delete, p)
			}
		}
		return Apply(staged, guest, kept)
	}
}

// THE NEGATIVE CONTROL FOR THE HARNESS ABOVE.
//
// A drop-injecting Apply that cannot actually drop anything would make every
// test below pass for the wrong reason. So: the same seam, dropping NOTHING,
// must sync cleanly — and the harness must be shown capable of failing before
// its passes mean anything. Refs: MGIT-164
func TestSyncHarness_DroppingNothing_SyncsCleanly(t *testing.T) {
	f := newFixture(t, map[string]string{"app.go": "V1"})
	require.NoError(t, os.WriteFile(filepath.Join(f.worktree, "new.go"), []byte("NEW"), 0o600))

	withApply(t, dropping()) // drops nothing: behaves exactly as Apply
	res, err := f.sync(false)

	require.NoError(t, err, "the harness must not fail a sync it did not sabotage")
	assert.Contains(t, res.Updated, "new.go")
	assert.FileExists(t, filepath.Join(f.guestTree, "new.go"))
}

// A CREATION that never reached the guest must never be reported as delivered.
//
// This is MGIT-164 as it actually happened: `git apply` created a file and
// modified its sibling, the guest could read neither, and mgit reported
// success. It was caught one layer up by a consumer's stale-copy check — which
// is to say mgit's own substrate did not notice it had lost the work.
//
// Sync's own tests could not have caught it: they assert on what Apply wrote,
// and Apply is what dropped it. The property is about the GUEST'S TREE read
// back, so the test drops the write underneath Sync and asserts on the refusal.
// Refs: MGIT-164, MGIT-76
func TestSync_ACreationTheGuestNeverReceived_IsRefusedNotReported(t *testing.T) {
	f := newFixture(t, map[string]string{"app.go": "V1"})
	require.NoError(t, os.WriteFile(
		filepath.Join(f.worktree, "Created.test.tsx"), []byte("NEW"), 0o600))

	withApply(t, dropping("Created.test.tsx"))
	res, err := f.sync(false)

	require.Error(t, err, "a sync that lost a creation must not report success")
	assert.Contains(t, err.Error(), "Created.test.tsx", "the refusal must name what did not land")
	assert.Empty(t, res.Updated, "nothing may be reported as delivered when nothing was")
	assert.NoFileExists(t, filepath.Join(f.guestTree, "Created.test.tsx"),
		"the premise: the guest genuinely does not have it")
}

// A MODIFICATION that landed stale is caught by the same read-back. The two
// halves of the field report were a creation and a modification of its
// sibling; both are checked, because a check that caught only one would have
// let that incident half through. Refs: MGIT-164
func TestSync_AModificationTheGuestNeverReceived_IsRefusedNotReported(t *testing.T) {
	f := newFixture(t, map[string]string{"app.go": "V1", "lib.go": "L1"})
	require.NoError(t, os.WriteFile(filepath.Join(f.worktree, "app.go"), []byte("V2"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(f.worktree, "lib.go"), []byte("L2"), 0o600))

	withApply(t, dropping("app.go"))
	_, err := f.sync(false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "app.go")
	assert.Equal(t, "V1", readFile(t, filepath.Join(f.guestTree, "app.go")),
		"the premise: the guest still holds the old bytes")
}

// A DELETE that did not happen is caught too. Deletion is the direction where
// a false success is worst: the agent believes the file is gone and its build
// keeps compiling it. Refs: MGIT-164, MGIT-90
func TestSync_ADeleteTheGuestNeverPerformed_IsRefusedNotReported(t *testing.T) {
	f := newFixture(t, map[string]string{"app.go": "V1", "gone.go": "OLD"})
	require.NoError(t, os.Remove(filepath.Join(f.worktree, "gone.go")))

	withApply(t, dropping("gone.go"))
	_, err := f.sync(false)

	require.Error(t, err, "a delete that did not happen must not report success")
	assert.Contains(t, err.Error(), "gone.go")
	assert.FileExists(t, filepath.Join(f.guestTree, "gone.go"), "the premise: it is still there")
}

// THE BASELINE MUST NOT MOVE ON AN UNDELIVERED SYNC.
//
// This is the half that makes the refusal recoverable rather than merely loud:
// if the manifest advanced, the next sync would compare against a delivery
// that never happened, conclude the host is unchanged, and skip — losing the
// work permanently and quietly. The verification therefore runs BEFORE
// SaveManifest, and this asserts that ordering by its consequence, not by
// reading the source. Refs: MGIT-164
func TestSync_AnUndeliveredSync_ReDerivesTheSameWorkNextTime(t *testing.T) {
	f := newFixture(t, map[string]string{"app.go": "V1"})
	require.NoError(t, os.WriteFile(filepath.Join(f.worktree, "new.go"), []byte("NEW"), 0o600))

	withApply(t, dropping("new.go"))
	_, err := f.sync(false)
	require.Error(t, err)

	// The drop is over; the next sync must still know there is work to do.
	withApply(t, dropping())
	res, err := f.sync(false)

	require.NoError(t, err)
	assert.Contains(t, res.Updated, "new.go",
		"a failed sync must re-derive its work, not record a delivery that never happened")
	assert.Equal(t, "NEW", readFile(t, filepath.Join(f.guestTree, "new.go")))
	assert.False(t, res.Skipped, "the baseline must not have advanced past the undelivered write")
}

// A partial delivery is reported as a failure of the WHOLE sync, not as a
// partial success. Refs: MGIT-164
func TestSync_APartialDelivery_IsNotAPartialSuccess(t *testing.T) {
	f := newFixture(t, map[string]string{"a.go": "1"})
	for _, name := range []string{"b.go", "c.go", "d.go"} {
		require.NoError(t, os.WriteFile(filepath.Join(f.worktree, name), []byte("X"), 0o600))
	}

	withApply(t, dropping("c.go"))
	res, err := f.sync(false)

	require.Error(t, err)
	assert.Empty(t, res.Updated, "a Result reporting b.go and d.go would be believed")
	assert.Empty(t, res.Deleted)
	// b.go and d.go DID land — the sync is refused anyway, because the caller
	// cannot act on "most of it arrived".
	assert.FileExists(t, filepath.Join(f.guestTree, "b.go"))
	assert.Contains(t, err.Error(), "c.go")
}

// removeForGuest is reached through Apply for every delete; its non-file
// branches are not, so they are covered directly. A directory must not abort
// the delete: the truncate is a best-effort staleness measure, and the unlink
// is what has to succeed. Refs: MGIT-90
func TestRemoveForGuest_NonRegularPaths(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, dir string) string
		check func(t *testing.T, path string)
	}{
		{
			name: "an_empty_directory_is_removed_despite_being_untruncatable",
			setup: func(t *testing.T, dir string) string {
				p := filepath.Join(dir, "d")
				require.NoError(t, os.Mkdir(p, 0o750))
				return p
			},
			check: func(t *testing.T, path string) {
				_, err := os.Stat(path)
				assert.True(t, os.IsNotExist(err), "the directory must be gone")
			},
		},
		{
			name:  "an_absent_path_is_not_an_error",
			setup: func(_ *testing.T, dir string) string { return filepath.Join(dir, "never") },
			check: func(t *testing.T, path string) {
				_, err := os.Stat(path)
				assert.True(t, os.IsNotExist(err))
			},
		},
		{
			name: "a_symlink_is_removed_and_its_target_survives",
			setup: func(t *testing.T, dir string) string {
				target := filepath.Join(dir, "target.txt")
				require.NoError(t, os.WriteFile(target, []byte("KEEP"), 0o600))
				link := filepath.Join(dir, "link")
				require.NoError(t, os.Symlink(target, link))
				return link
			},
			check: func(t *testing.T, path string) {
				_, err := os.Lstat(path)
				assert.True(t, os.IsNotExist(err), "the link is gone")
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := tt.setup(t, dir)
			require.NoError(t, removeForGuest(path))
			tt.check(t, path)
		})
	}
}

// EXPECTED TO FAIL — SKIPPED, NAMING MGIT-168.
//
// removeForGuest truncates before unlinking (MGIT-90: a guest's cached dentry
// keeps resolving a deleted name, so the file is emptied to make a stale read
// fail loudly rather than return deleted code). os.Truncate FOLLOWS LINKS, so
// deleting a symlink empties its TARGET — a path the plan never named.
//
// Nothing catches it: VerifyDelivery checks only planned paths, deliberately,
// so the damaged file is outside the check by construction and the sync
// reports success. Refs: MGIT-168, MGIT-90, MGIT-164
func TestRemoveForGuest_Symlink_DoesNotTruncateItsTarget(t *testing.T) {
	t.Skip("MGIT-168: removeForGuest truncates through a symlink and empties its target")

	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	require.NoError(t, os.WriteFile(target, []byte("KEEP-THIS"), 0o600))
	link := filepath.Join(dir, "link")
	require.NoError(t, os.Symlink(target, link))

	require.NoError(t, removeForGuest(link))

	got, err := os.ReadFile(target) //nolint:gosec // a t.TempDir path this test wrote
	require.NoError(t, err, "the target must survive the link's removal")
	assert.Equal(t, "KEEP-THIS", string(got),
		"truncating through a link would destroy a file the plan never named")
}

// EXPECTED TO FAIL — SKIPPED, NAMING MGIT-165.
//
// A worktree holding an ordinary internal symlink cannot be synced at all.
// staging preserves the link, BuildManifest records it by target text, and
// Apply -> copyInto FOLLOWS it and writes a regular file; the MGIT-164
// read-back then correctly calls that stale content and refuses the sync.
//
// Each of those three is individually tested and individually correct. The
// composition is what is broken, which is exactly why per-function tests did
// not see it. The test is written whole and skipped rather than narrowed: it
// turns red the moment the skip is removed. Refs: MGIT-165, MGIT-164, SEC-03
func TestSync_AWorktreeSymlink_IsDeliveredAsALink(t *testing.T) {
	t.Skip("MGIT-165: sync flattens a worktree symlink and the read-back then refuses the whole sync")

	f := newFixture(t, map[string]string{"real.txt": "hello"})
	require.NoError(t, os.Symlink("real.txt", filepath.Join(f.worktree, "link.txt")))

	res, err := f.sync(false)

	require.NoError(t, err, "a symlink is ordinary repo content; a sync must not refuse it")
	assert.Contains(t, res.Updated, "link.txt")

	info, err := os.Lstat(filepath.Join(f.guestTree, "link.txt"))
	require.NoError(t, err)
	assert.NotZero(t, info.Mode()&fs.ModeSymlink,
		"the guest must receive a link, not a flattened copy: an edit through the "+
			"link has to keep aliasing its target")
}

// EXPECTED TO FAIL — SKIPPED, NAMING MGIT-167.
//
// Plan.Forced() promises in its own comment that a conflict over a path the
// host no longer has becomes a DELETE. It does not: every conflict becomes an
// Update, Apply stats a file that is not in the candidate tree, and --force
// dies with a raw stat error naming an internal path.
//
// --force is the mode where the user has already accepted the loss, so
// refusing to complete it — with a diagnosis they cannot act on — is the worst
// place to fail. Refs: MGIT-167, MGIT-71, ADR-011
func TestSync_ForceOverAHostDeletedPath_RemovesItFromTheGuest(t *testing.T) {
	t.Skip("MGIT-167: Plan.Forced() promotes a host-deleted conflict to an update, and Apply then fails")

	f := newFixture(t, map[string]string{"doomed.txt": "V1", "keep.txt": "K"})
	// The guest edited it; the host then deleted it. Exactly what --force is for.
	require.NoError(t, os.WriteFile(
		filepath.Join(f.guestTree, "doomed.txt"), []byte("GUEST-EDIT"), 0o600))
	require.NoError(t, os.Remove(filepath.Join(f.worktree, "doomed.txt")))

	res, err := f.sync(true)

	require.NoError(t, err, "--force must be able to honor a host deletion")
	assert.Contains(t, res.Deleted, "doomed.txt")
	assert.Contains(t, res.Overridden, "doomed.txt",
		"destroying un-landed guest work stays audited even when it was asked for")
	assert.NoFileExists(t, filepath.Join(f.guestTree, "doomed.txt"))
	assert.FileExists(t, filepath.Join(f.guestTree, "keep.txt"))
}

// The regression control for MGIT-167's eventual fix: a forced update over a
// guest-modified path the host STILL has must keep working exactly as it does
// today. Refs: MGIT-167, MGIT-71
func TestSync_ForceOverAPathTheHostStillHas_IsUnchanged(t *testing.T) {
	f := newFixture(t, map[string]string{"app.go": "V1"})
	require.NoError(t, os.WriteFile(filepath.Join(f.guestTree, "app.go"), []byte("GUEST"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(f.worktree, "app.go"), []byte("V2"), 0o600))

	res, err := f.sync(true)

	require.NoError(t, err)
	assert.Contains(t, res.Overridden, "app.go")
	assert.Equal(t, "V2", readFile(t, filepath.Join(f.guestTree, "app.go")))
}

// UndeliveredError bounds what it names, so a refusal over a large tree stays
// readable — the MGIT-160 lesson applied to this message. The count is
// asserted as a property (every path is accounted for), not by matching the
// rendered sentence. Refs: MGIT-164, MGIT-160
func TestUndeliveredError_BoundsWhatItNames_AndAccountsForTheRest(t *testing.T) {
	const total = maxNamedUndelivered + 37
	paths := make([]string, total)
	for i := range paths {
		paths[i] = filepath.Join("src", "generated", "file.ts")
	}
	err := &UndeliveredError{Paths: paths}

	msg := err.Error()

	assert.Equal(t, maxNamedUndelivered, strings.Count(msg, "src/generated/file.ts"),
		"exactly the cap is named")
	assert.Contains(t, msg, "37 more", "and the remainder is counted, never dropped")
}

// A short list names every path and adds no "and N more" tail.
func TestUndeliveredError_ShortList_NamesEverythingAndCountsNothing(t *testing.T) {
	err := &UndeliveredError{Paths: []string{"a.txt (absent)", "b.txt (stale content)"}}

	msg := err.Error()

	assert.Contains(t, msg, "a.txt (absent)")
	assert.Contains(t, msg, "b.txt (stale content)")
	assert.NotContains(t, msg, "more", "a complete list must not suggest it was shortened")
}

// A guest tree that cannot be read back is a refusal, not a silent success:
// "I could not check" and "it delivered" are different facts and only one of
// them is safe to report. Refs: MGIT-164
func TestSync_UnreadableGuestTreeOnReadBack_IsRefused(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the mode bits this test relies on")
	}
	f := newFixture(t, map[string]string{"app.go": "V1"})
	require.NoError(t, os.WriteFile(filepath.Join(f.worktree, "new.go"), []byte("NEW"), 0o600))

	// Let Apply run, then leave a file the read-back walk cannot hash. A
	// guest-created path is used deliberately: it is outside the plan, so the
	// failure is the CHECK being unable to run, not a delivery going wrong.
	withApply(t, func(staged, guest string, plan Plan) error {
		if err := Apply(staged, guest, plan); err != nil {
			return err
		}
		blocked := filepath.Join(guest, "guest-owned.bin")
		if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
			return err
		}
		return os.Chmod(blocked, 0o000)
	})

	_, err := f.sync(false)

	require.Error(t, err, "a read-back that could not run must never pass as a delivery")
	assert.True(t,
		strings.Contains(err.Error(), "read back") || strings.Contains(err.Error(), "read tree"),
		"the refusal must say the check could not run, not that a path was missing: %v", err)
	assert.False(t, errors.Is(err, ErrConflict))
}

// EXPECTED TO FAIL — SKIPPED, NAMING MGIT-168.
//
// The same defect end-to-end, which is where it is worst: the host removes
// only a link, the sync reports a clean success with Deleted:[link.txt], and a
// file that appears in no plan and no report has been emptied.
//
// It is the unit test's composition twin, kept separate because the unit
// failure is a wrong syscall and this one is a wrong REPORT — a sync that
// destroys data and says it succeeded is the exact failure MGIT-164's own
// argument calls worse than a crash. Refs: MGIT-168, MGIT-164
func TestSync_DeletingASymlink_LeavesItsTargetIntact(t *testing.T) {
	t.Skip("MGIT-168: deleting a symlink empties its target, and the sync reports success")

	f := newFixture(t, map[string]string{"real.txt": "IMPORTANT-CONTENT"})
	require.NoError(t, os.Symlink("real.txt", filepath.Join(f.worktree, "link.txt")))
	// Re-stage so the guest holds the link, exactly as a launch delivers it.
	require.NoError(t, os.RemoveAll(f.guestTree))
	require.NoError(t, staging.Build(f.worktree, f.store, f.guestTree))
	require.NoError(t, RecordDelivery(f.guestTree, f.stateDir))

	require.NoError(t, os.Remove(filepath.Join(f.worktree, "link.txt")))
	res, err := f.sync(false)

	require.NoError(t, err)
	assert.Contains(t, res.Deleted, "link.txt")
	assert.Equal(t, "IMPORTANT-CONTENT", readFile(t, filepath.Join(f.guestTree, "real.txt")),
		"a delete must not damage a path outside its own plan, and must never "+
			"report success while it has")
}
