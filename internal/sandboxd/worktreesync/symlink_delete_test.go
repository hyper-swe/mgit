package worktreesync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/sandboxd/staging"
)

// TestRemoveForGuest_Symlink_RemovesTheLinkAndLeavesItsTargetIntact pins the
// MGIT-168 boundary at the syscall: deleting a LINK must never touch the
// file it points at.
//
// removeForGuest empties a file before unlinking it (MGIT-90), and
// os.Truncate follows symlinks — so a planned delete of link.txt landed the
// truncate on link.txt's TARGET, a path the plan never named, and the target
// survived as a zero-byte file. The rows are every non-regular shape a
// delivered path can have, plus the regular-file control that keeps the
// MGIT-90 property honest: the table is drawn from the kinds of entry a
// manifest can hold, not from the implementation.
// Refs: MGIT-168, MGIT-90, MGIT-164
func TestRemoveForGuest_Symlink_RemovesTheLinkAndLeavesItsTargetIntact(t *testing.T) {
	const keep = "KEEP-THIS"
	tests := []struct {
		name string
		// setup lays out dir and returns the path the plan names.
		setup func(t *testing.T, dir string) string
		// check asserts on what must survive, after the delete.
		check func(t *testing.T, dir, path string)
	}{
		{
			name: "link_with_a_relative_target",
			setup: func(t *testing.T, dir string) string {
				require.NoError(t, os.WriteFile(filepath.Join(dir, "real.txt"), []byte(keep), 0o600))
				link := filepath.Join(dir, "link.txt")
				require.NoError(t, os.Symlink("real.txt", link))
				return link
			},
			check: func(t *testing.T, dir, _ string) {
				assert.Equal(t, keep, readFile(t, filepath.Join(dir, "real.txt")),
					"the target's contents must survive the link's removal")
			},
		},
		{
			name: "link_with_an_absolute_target",
			setup: func(t *testing.T, dir string) string {
				target := filepath.Join(dir, "real.txt")
				require.NoError(t, os.WriteFile(target, []byte(keep), 0o600))
				link := filepath.Join(dir, "link.txt")
				require.NoError(t, os.Symlink(target, link))
				return link
			},
			check: func(t *testing.T, dir, _ string) {
				assert.Equal(t, keep, readFile(t, filepath.Join(dir, "real.txt")))
			},
		},
		{
			name: "link_in_a_subdirectory_pointing_up_the_tree",
			setup: func(t *testing.T, dir string) string {
				require.NoError(t, os.WriteFile(filepath.Join(dir, "real.txt"), []byte(keep), 0o600))
				require.NoError(t, os.MkdirAll(filepath.Join(dir, "docs"), 0o750))
				link := filepath.Join(dir, "docs", "link.txt")
				require.NoError(t, os.Symlink(filepath.Join("..", "real.txt"), link))
				return link
			},
			check: func(t *testing.T, dir, _ string) {
				assert.Equal(t, keep, readFile(t, filepath.Join(dir, "real.txt")))
			},
		},
		{
			name: "link_to_a_directory",
			setup: func(t *testing.T, dir string) string {
				require.NoError(t, os.MkdirAll(filepath.Join(dir, "pkg"), 0o750))
				require.NoError(t, os.WriteFile(filepath.Join(dir, "pkg", "a.go"), []byte(keep), 0o600))
				link := filepath.Join(dir, "pkg-link")
				require.NoError(t, os.Symlink("pkg", link))
				return link
			},
			check: func(t *testing.T, dir, _ string) {
				assert.Equal(t, keep, readFile(t, filepath.Join(dir, "pkg", "a.go")),
					"the directory behind the link must be untouched")
			},
		},
		{
			name: "dangling_link",
			setup: func(t *testing.T, dir string) string {
				link := filepath.Join(dir, "dangling")
				require.NoError(t, os.Symlink("never-created.txt", link))
				return link
			},
			check: func(*testing.T, string, string) {},
		},
		{
			name: "regular_file_control_is_emptied_then_removed",
			setup: func(t *testing.T, dir string) string {
				path := filepath.Join(dir, "doomed.txt")
				require.NoError(t, os.WriteFile(path, []byte("OLD"), 0o600))
				return path
			},
			check: func(*testing.T, string, string) {},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := tt.setup(t, dir)

			require.NoError(t, removeForGuest(path))

			_, err := os.Lstat(path)
			assert.True(t, os.IsNotExist(err), "the planned path itself must be gone: %v", err)
			tt.check(t, dir, path)
		})
	}
}

// TestSync_HostDeletesASymlink_TargetSurvivesAndTheReportNamesOnlyTheLink is
// the same defect end to end, through Sync and the MGIT-164 read-back, which
// is where it was worst: the host removed only a link, the sync reported a
// clean success naming link.txt, and a file that appeared in no plan and no
// report had been emptied.
//
// The read-back is on the path deliberately. VerifyDelivery checks only
// planned paths — a guest is entitled to its own files — so the damaged
// target was outside the check by construction and the report stayed clean.
// The fix has to hold at the syscall; this test proves the whole verb, read-
// back included, then tells the truth about it: a correct report, an intact
// target, and a baseline that moved only after that was verified.
// Refs: MGIT-168, MGIT-164, MGIT-90
func TestSync_HostDeletesASymlink_TargetSurvivesAndTheReportNamesOnlyTheLink(t *testing.T) {
	const keep = "IMPORTANT-CONTENT"
	f := newFixture(t, map[string]string{"real.txt": keep})
	require.NoError(t, os.Symlink("real.txt", filepath.Join(f.worktree, "link.txt")))
	// Re-stage so the guest holds the link exactly as a launch delivers it,
	// and the baseline records it.
	require.NoError(t, os.RemoveAll(f.guestTree))
	require.NoError(t, staging.Build(f.worktree, f.store, f.guestTree))
	require.NoError(t, RecordDelivery(f.guestTree, f.stateDir))
	require.Equal(t, keep, readFile(t, filepath.Join(f.guestTree, "real.txt")), "precondition: the guest holds the target")

	// The host deletes ONLY the link.
	require.NoError(t, os.Remove(filepath.Join(f.worktree, "link.txt")))
	res, err := f.sync(false)

	require.NoError(t, err)
	assert.Equal(t, []string{"link.txt"}, res.Deleted, "the report names the link and nothing else")
	assert.Empty(t, res.Updated)
	_, err = os.Lstat(filepath.Join(f.guestTree, "link.txt"))
	assert.True(t, os.IsNotExist(err), "the link is gone from the guest")
	assert.Equal(t, keep, readFile(t, filepath.Join(f.guestTree, "real.txt")),
		"a delete must not damage a path outside its own plan, and must never report success while it has")

	// The baseline moved only after the read-back verified the delete: the
	// next sync has nothing to carry.
	again, err := f.sync(false)
	require.NoError(t, err)
	assert.True(t, again.Skipped, "the verified delete is the new baseline")
}
