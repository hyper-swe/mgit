package worktreesync

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/sandboxd/staging"
)

// lstatMode returns the entry's own mode (a link is reported as a link).
func lstatMode(t *testing.T, path string) fs.FileMode {
	t.Helper()
	info, err := os.Lstat(path)
	require.NoError(t, err)
	return info.Mode()
}

// TestSync_AWorktreeSymlink_ReachesTheGuestAsALink is MGIT-165 at the syncer
// layer, where it lived: staging preserves an in-tree link, BuildManifest
// records it by target text, and Apply -> copyInto FOLLOWED it and wrote a
// regular file holding the target's bytes — so the MGIT-164 read-back
// correctly called that stale content and refused the whole sync. Three
// per-function tests each passed while the composition was broken, which is
// why the assertion is on the whole verb.
//
// The rows are the ways a link can change on the host — appear, retarget,
// replace a file, be replaced by one, sit in a subdirectory — and the guest
// must end up holding what the host holds, as a LINK. In every row the file a
// link points at keeps its own contents: a link is delivered by recreating it,
// never by writing THROUGH the one the guest already has. Refs: MGIT-165,
// MGIT-164, SEC-03
func TestSync_AWorktreeSymlink_ReachesTheGuestAsALink(t *testing.T) {
	tests := []struct {
		name string
		// launch is what the worktree holds when the sandbox launches.
		launch map[string]string
		links  map[string]string // link -> target, present at launch
		// change edits the host worktree after launch.
		change func(t *testing.T, wt string)
		// wantUpdated is the path the report must name.
		wantUpdated string
		// wantLink is the target the guest's link must carry afterwards; ""
		// means the guest path must be a regular file holding wantContent.
		wantLink    string
		wantContent string
		// intact maps guest paths to the contents they must still hold.
		intact map[string]string
	}{
		{
			name:   "a_new_link_is_delivered_as_a_link",
			launch: map[string]string{"real.txt": "hello"},
			change: func(t *testing.T, wt string) {
				require.NoError(t, os.Symlink("real.txt", filepath.Join(wt, "link.txt")))
			},
			wantUpdated: "link.txt", wantLink: "real.txt",
			intact: map[string]string{"real.txt": "hello"},
		},
		{
			name:   "a_retargeted_link_is_replaced_not_written_through",
			launch: map[string]string{"a.txt": "AAA", "b.txt": "BBB"},
			links:  map[string]string{"link.txt": "a.txt"},
			change: func(t *testing.T, wt string) {
				require.NoError(t, os.Remove(filepath.Join(wt, "link.txt")))
				require.NoError(t, os.Symlink("b.txt", filepath.Join(wt, "link.txt")))
			},
			wantUpdated: "link.txt", wantLink: "b.txt",
			intact: map[string]string{"a.txt": "AAA", "b.txt": "BBB"},
		},
		{
			name:   "a_file_that_became_a_link_is_delivered_as_a_link",
			launch: map[string]string{"cfg": "OLD", "real.cfg": "REAL"},
			change: func(t *testing.T, wt string) {
				require.NoError(t, os.Remove(filepath.Join(wt, "cfg")))
				require.NoError(t, os.Symlink("real.cfg", filepath.Join(wt, "cfg")))
			},
			wantUpdated: "cfg", wantLink: "real.cfg",
			intact: map[string]string{"real.cfg": "REAL"},
		},
		{
			name:   "a_link_that_became_a_file_is_delivered_as_a_file_and_its_old_target_is_untouched",
			launch: map[string]string{"real.txt": "REAL"},
			links:  map[string]string{"link.txt": "real.txt"},
			change: func(t *testing.T, wt string) {
				require.NoError(t, os.Remove(filepath.Join(wt, "link.txt")))
				require.NoError(t, os.WriteFile(filepath.Join(wt, "link.txt"), []byte("NOW-A-FILE"), 0o600))
			},
			wantUpdated: "link.txt", wantContent: "NOW-A-FILE",
			intact: map[string]string{"real.txt": "REAL"},
		},
		{
			name:   "a_link_in_a_subdirectory_pointing_up_is_delivered_as_a_link",
			launch: map[string]string{"README.md": "top"},
			change: func(t *testing.T, wt string) {
				require.NoError(t, os.MkdirAll(filepath.Join(wt, "docs"), 0o750))
				require.NoError(t, os.Symlink(filepath.Join("..", "README.md"), filepath.Join(wt, "docs", "README.md")))
			},
			wantUpdated: filepath.Join("docs", "README.md"), wantLink: filepath.Join("..", "README.md"),
			intact: map[string]string{"README.md": "top"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, tt.launch)
			for link, target := range tt.links {
				require.NoError(t, os.Symlink(target, filepath.Join(f.worktree, link)))
			}
			if len(tt.links) > 0 {
				// Re-stage so the guest holds the links exactly as a launch
				// delivers them, and the baseline records them.
				require.NoError(t, os.RemoveAll(f.guestTree))
				require.NoError(t, staging.Build(f.worktree, f.store, f.guestTree))
				require.NoError(t, RecordDelivery(f.guestTree, f.stateDir))
			}

			tt.change(t, f.worktree)
			res, err := f.sync(false)

			require.NoError(t, err, "a symlink is ordinary repo content; a sync must not refuse it")
			assert.Equal(t, []string{tt.wantUpdated}, res.Updated)
			guestPath := filepath.Join(f.guestTree, tt.wantUpdated)
			if tt.wantLink != "" {
				require.NotZero(t, lstatMode(t, guestPath)&fs.ModeSymlink,
					"the guest must receive a LINK, not a flattened copy: an edit through it has to keep aliasing the target")
				got, err := os.Readlink(guestPath)
				require.NoError(t, err)
				assert.Equal(t, tt.wantLink, got, "the link text is delivered verbatim")
			} else {
				require.Zero(t, lstatMode(t, guestPath)&fs.ModeSymlink, "the guest must now hold a regular file")
				assert.Equal(t, tt.wantContent, readFile(t, guestPath))
			}
			for rel, want := range tt.intact {
				assert.Equal(t, want, readFile(t, filepath.Join(f.guestTree, rel)),
					"%s must keep its own contents: a link is recreated, never written through", rel)
			}
			// The read-back agreed with what was staged, so the baseline moved.
			again, err := f.sync(false)
			require.NoError(t, err)
			assert.True(t, again.Skipped, "the delivered link is the new baseline")
		})
	}
}

// TestSync_EscapingSymlink_IsStillRefused_Control is the negative control for
// the table above: delivering links as links must not have widened what a
// link may point at. The refusal is staging's, before any copy. Refs: SEC-03,
// MGIT-165
func TestSync_EscapingSymlink_IsStillRefused_Control(t *testing.T) {
	f := newFixture(t, map[string]string{"app.go": "V1"})
	outside := filepath.Join(t.TempDir(), "secret")
	require.NoError(t, os.WriteFile(outside, []byte("HOST-SECRET"), 0o600))
	require.NoError(t, os.Symlink(outside, filepath.Join(f.worktree, "leak")))

	_, err := f.sync(false)

	require.ErrorIs(t, err, staging.ErrSymlinkEscape)
	_, statErr := os.Lstat(filepath.Join(f.guestTree, "leak"))
	assert.True(t, os.IsNotExist(statErr), "nothing about the escaping link reaches the guest")
	assert.Equal(t, "HOST-SECRET", readFile(t, outside))
}

// TestSync_Force_OverAGuestPlantedLink_ReplacesItAndNeverWritesThroughIt is
// the hostile form of the same seam. The guest owns its tree and can replace a
// delivered file with a link to any path it likes; copyInto opened the
// destination with O_TRUNC, which follows links, so a forced update of that
// path would have emptied and overwritten whatever the guest pointed it at —
// resolved on the HOST, by the daemon. The injected "outside" file stands in
// for that host path; nothing here touches a real one. Refs: MGIT-165, SEC-03
func TestSync_Force_OverAGuestPlantedLink_ReplacesItAndNeverWritesThroughIt(t *testing.T) {
	f := newFixture(t, map[string]string{"app.go": "V1"})
	outside := filepath.Join(t.TempDir(), "host-file")
	require.NoError(t, os.WriteFile(outside, []byte("HOST-CONTENT"), 0o600))
	// The guest swaps the delivered file for a link to the outside path.
	require.NoError(t, os.Remove(filepath.Join(f.guestTree, "app.go")))
	require.NoError(t, os.Symlink(outside, filepath.Join(f.guestTree, "app.go")))
	// The host edits the same path; the guest's swap makes it a conflict.
	writeTree(t, f.worktree, map[string]string{"app.go": "V2"})

	_, err := f.sync(false)
	require.ErrorIs(t, err, ErrConflict, "unforced, the guest's change blocks the sync")
	assert.Equal(t, "HOST-CONTENT", readFile(t, outside))

	res, err := f.sync(true)

	// The outside file is asserted FIRST and unconditionally: on the unfixed
	// code the sync is refused by the read-back AND the outside file has
	// already been overwritten — a refusal that arrives after the damage is
	// not protection, and the receipt has to show both.
	assert.Equal(t, "HOST-CONTENT", readFile(t, outside),
		"a forced update must replace the guest's link, never write through it to what it points at")
	require.NoError(t, err)
	assert.Equal(t, []string{"app.go"}, res.Overridden)
	guestPath := filepath.Join(f.guestTree, "app.go")
	require.Zero(t, lstatMode(t, guestPath)&fs.ModeSymlink, "the guest path is the host's regular file again")
	assert.Equal(t, "V2", readFile(t, guestPath))
}
