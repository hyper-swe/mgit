package staging

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// symlinkedRoot returns two paths to the SAME directory: one reached directly,
// one reached through a symlinked parent.
//
// It is built explicitly rather than relying on the host's layout. macOS's
// /tmp and /var/folders are symlinks and Linux's usually are not, so a test
// that took the platform's word for it would assert different things on
// different machines — and this boundary is precisely where that difference
// decides the answer.
func symlinkedRoot(t *testing.T) (direct, viaLink string) {
	t.Helper()
	// Canonicalize the temp base first: on macOS t.TempDir already sits under
	// /var -> /private/var, which would make "direct" a symlinked path too and
	// collapse the two cases this helper exists to separate.
	base, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)

	realParent := filepath.Join(base, "real")
	direct = filepath.Join(realParent, "wt")
	require.NoError(t, os.MkdirAll(direct, 0o750))

	// The symlink is a PARENT of the worktree, not the worktree itself — the
	// /tmp -> /private/tmp shape, where the final component is an ordinary
	// directory reached through a linked prefix. A root that IS a link is a
	// different (and unreachable) case: filepath.WalkDir does not descend one.
	require.NoError(t, os.Symlink(realParent, filepath.Join(base, "alias")))
	viaLink = filepath.Join(base, "alias", "wt")
	return direct, viaLink
}

// AssertSymlinkWithin's doc comment promises, in so many words:
//
//	A link to a not-yet-existing in-root path is allowed; one pointing
//	outside root (absolute or via ..) is rejected.
//
// The table below is drawn from that sentence and from SEC-03's statement of
// the invariant — NOT from the implementation, which is the thing under test.
//
// Both halves are asserted together on purpose. A containment check has two
// ways to be wrong and only one of them is loud: letting an escape through is
// caught the first time someone looks, while refusing safe content produces a
// security-sounding error about a breach that does not exist, and the reader
// goes hunting. Refs: SEC-03, F-A/NEW-2, MGIT-166
func TestAssertSymlinkWithin_TheBoundaryItsDocPromises(t *testing.T) {
	tests := []struct {
		name string
		// setup returns the link's target, given the root it will live in.
		setup      func(t *testing.T, root string) string
		rootViaLnk bool // reach the worktree root through a symlinked parent
		wantEscape bool
		skipIssue  string
	}{
		{
			name:  "a_link_to_an_existing_in_root_file_is_allowed",
			setup: func(t *testing.T, root string) string { return writeIn(t, root, "real.txt") },
		},
		{
			name:       "a_link_to_an_existing_in_root_file_is_allowed_through_a_symlinked_root",
			setup:      func(t *testing.T, root string) string { return writeIn(t, root, "real.txt") },
			rootViaLnk: true,
		},
		{
			name:  "a_link_to_a_NOT_YET_EXISTING_in_root_path_is_allowed",
			setup: func(*testing.T, string) string { return "generated-later.txt" },
		},
		{
			name:       "a_link_to_a_NOT_YET_EXISTING_in_root_path_is_allowed_through_a_symlinked_root",
			setup:      func(*testing.T, string) string { return "generated-later.txt" },
			rootViaLnk: true,
		},
		{
			name:       "an_absolute_target_outside_the_root_is_rejected",
			setup:      func(t *testing.T, _ string) string { return writeIn(t, t.TempDir(), "secret") },
			wantEscape: true,
		},
		{
			name: "a_dotdot_target_that_leaves_the_root_is_rejected",
			setup: func(t *testing.T, root string) string {
				outside := writeIn(t, t.TempDir(), "secret")
				rel, err := filepath.Rel(root, outside)
				require.NoError(t, err)
				require.True(t, strings.HasPrefix(rel, ".."+string(filepath.Separator)),
					"the target must genuinely leave the root, got %q", rel)
				return rel
			},
			wantEscape: true,
		},
		{
			name: "a_CHAIN_of_links_cannot_step_outside_one_hop_at_a_time",
			setup: func(t *testing.T, root string) string {
				// hop lives inside the root and points outside it. A check that
				// only looked at the immediate target would call this in-root.
				outside := writeIn(t, t.TempDir(), "secret")
				hop := filepath.Join(root, "hop")
				require.NoError(t, os.Symlink(outside, hop))
				return "hop"
			},
			wantEscape: true,
		},
		{
			name:       "an_absolute_target_INSIDE_the_root_is_allowed",
			setup:      func(t *testing.T, root string) string { return writeIn(t, root, "inside.txt") },
			wantEscape: false,
		},
		{
			name:       "a_link_to_the_root_itself_is_allowed",
			setup:      func(_ *testing.T, root string) string { return root },
			wantEscape: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.skipIssue != "" {
				t.Skip(tt.skipIssue)
			}
			direct, viaLink := symlinkedRoot(t)
			root := direct
			if tt.rootViaLnk {
				root = viaLink
			}
			target := tt.setup(t, root)
			link := filepath.Join(root, "the-link")
			require.NoError(t, os.Symlink(target, link))

			err := AssertSymlinkWithin(root, link)

			if tt.wantEscape {
				require.ErrorIs(t, err, ErrSymlinkEscape,
					"a target outside the worktree must fail closed with the sentinel")
				assert.Contains(t, err.Error(), target,
					"the refusal must name the target, or the operator cannot find the link")
				return
			}
			require.NoError(t, err,
				"refusing safe content is not the safe direction: it reports a containment "+
					"breach that does not exist")
		})
	}
}

// EXPECTED TO FAIL — SKIPPED, NAMING MGIT-166.
//
// The whole-build consequence of the case skipped above. staging fails closed
// on the first offending link, so one link to a not-yet-generated in-tree path
// refuses the entire launch or sync — and CLAUDE.md's own instruction puts
// every dogfooded worktree under /tmp, which on macOS is a symlink.
// Refs: MGIT-166, SEC-03
func TestBuild_ALinkToANotYetGeneratedPath_DoesNotFailTheBuild(t *testing.T) {

	_, wt := symlinkedRoot(t)
	require.NoError(t, os.WriteFile(filepath.Join(wt, "main.go"), []byte("package main"), 0o600))
	// The shape a build produces: dist is generated, the link is checked in.
	require.NoError(t, os.Symlink("build/out", filepath.Join(wt, "dist")))

	stage := filepath.Join(t.TempDir(), "stage")
	err := Build(wt, privateStoreWith(t), stage)

	require.NoError(t, err, "a link to a path the build has not produced yet is not an escape")
	fi, lerr := os.Lstat(filepath.Join(stage, "dist"))
	require.NoError(t, lerr)
	assert.NotZero(t, fi.Mode()&os.ModeSymlink, "and it is delivered as a link")
}

// The negative control for the case above: the SAME build, through the SAME
// helper, with a genuinely escaping link must still be refused. Without it, a
// future "fix" that simply stopped checking would make the skipped test pass.
// Refs: MGIT-166, SEC-03
func TestBuild_ThroughASymlinkedRoot_StillRefusesARealEscape(t *testing.T) {
	_, wt := symlinkedRoot(t)
	outside := writeIn(t, t.TempDir(), "host-secret")
	require.NoError(t, os.Symlink(outside, filepath.Join(wt, "escape")))

	err := Build(wt, privateStoreWith(t), filepath.Join(t.TempDir(), "stage"))

	require.ErrorIs(t, err, ErrSymlinkEscape,
		"canonicalizing the root must not weaken the check it exists to serve")
}

// A store directory is a repository boundary WHEREVER it sits — the MGIT-157
// lesson, asserted here in staging.
//
// The names are written as literals rather than read from GuestStoreName. A
// case list drawn from the constant the subject exports would keep agreeing
// with the subject if the constant itself were wrong; ".git" and ".mgit" are
// facts about git and about ADR-001, not about this package.
// Refs: SEC-03, MGIT-157, MGIT-14
func TestBuild_StoreDirectories_AreDroppedAtEveryDepth(t *testing.T) {
	for _, store := range []string{".git", ".mgit"} {
		for _, depth := range []string{"", "vendor", "a/b/c"} {
			name := store + "_at_" + depth
			if depth == "" {
				name = store + "_at_the_root"
			}
			t.Run(name, func(t *testing.T) {
				wt := t.TempDir()
				dir := filepath.Join(wt, depth, store)
				require.NoError(t, os.MkdirAll(dir, 0o750))
				require.NoError(t, os.WriteFile(filepath.Join(dir, "HEAD"), []byte("LEAK"), 0o600))
				keep := filepath.Join(wt, depth, "keep.txt")
				require.NoError(t, os.WriteFile(keep, []byte("keep"), 0o600))

				stage := filepath.Join(t.TempDir(), "stage")
				require.NoError(t, Build(wt, privateStoreWith(t), stage))

				leaked := filepath.Join(stage, depth, store, "HEAD")
				if got, rerr := os.ReadFile(leaked); rerr == nil { //nolint:gosec // test-owned staging dir
					// A .mgit at the ROOT is where the private store is laid
					// in, so the path legitimately exists there. The claim is
					// about whose bytes are in it.
					assert.NotContains(t, string(got), "LEAK",
						"another repository's history must never reach the guest")
				}
				assert.FileExists(t, filepath.Join(stage, depth, "keep.txt"),
					"its innocent siblings must survive")
			})
		}
	}
}

// The private store is the ONE .mgit the guest gets, and it is laid in after
// the worktree walk — so a worktree that carried its own .mgit must not be
// able to shadow it. Refs: SEC-03, MGIT-14
func TestBuild_AWorktreeOwnMgit_CannotShadowThePrivateStore(t *testing.T) {
	wt := t.TempDir()
	rogue := filepath.Join(wt, ".mgit")
	require.NoError(t, os.MkdirAll(rogue, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(rogue, "HEAD"), []byte("ROGUE"), 0o600))

	stage := filepath.Join(t.TempDir(), "stage")
	require.NoError(t, Build(wt, privateStoreWith(t), stage))

	got, err := os.ReadFile(filepath.Join(stage, ".mgit", "HEAD")) //nolint:gosec // test-owned staging dir
	require.NoError(t, err)
	assert.NotContains(t, string(got), "ROGUE",
		"the guest's only store is the private one the host laid in")
}

// copyTree carries the private store in verbatim, symlinks included. It is a
// separate walk from the worktree one and has its own trust argument (the host
// built the store), so it gets its own test rather than being assumed.
func TestBuild_PrivateStore_IsCopiedVerbatimIncludingLinksAndNesting(t *testing.T) {
	store := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(store, "refs", "heads"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(store, "refs", "heads", "task"),
		[]byte("deadbeef"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(store, "HEAD"),
		[]byte("ref: refs/heads/task"), 0o600))
	require.NoError(t, os.Symlink("HEAD", filepath.Join(store, "HEAD-alias")))

	stage := filepath.Join(t.TempDir(), "stage")
	require.NoError(t, Build(t.TempDir(), store, stage))

	assert.FileExists(t, filepath.Join(stage, ".mgit", "refs", "heads", "task"),
		"a nested store path survives the copy")
	fi, err := os.Lstat(filepath.Join(stage, ".mgit", "HEAD-alias"))
	require.NoError(t, err)
	assert.NotZero(t, fi.Mode()&os.ModeSymlink,
		"a host-built store's own links are carried verbatim, not flattened")
}

// Build fails CLOSED: one escaping link aborts the whole build, and the caller
// must never be handed a usable-looking partial tree. The property asserted is
// that the ERROR is returned — what remains on disk is documented as the
// caller's to remove. Refs: SEC-03
func TestBuild_OneEscapingLink_AbortsTheWholeBuild(t *testing.T) {
	wt := t.TempDir()
	for _, n := range []string{"a.txt", "b.txt", "c.txt"} {
		require.NoError(t, os.WriteFile(filepath.Join(wt, n), []byte("x"), 0o600))
	}
	outside := writeIn(t, t.TempDir(), "secret")
	require.NoError(t, os.Symlink(outside, filepath.Join(wt, "escape")))

	stage := filepath.Join(t.TempDir(), "stage")
	err := Build(wt, privateStoreWith(t), stage)

	require.ErrorIs(t, err, ErrSymlinkEscape)
	assert.NoFileExists(t, filepath.Join(stage, "escape"),
		"the offending link is never materialized")
	assert.NoFileExists(t, filepath.Join(stage, ".mgit", "HEAD"),
		"and the build stopped before laying the store in: nothing here is deliverable")
}

// A directory symlink is not followed by the walk, so it is delivered as a
// link rather than as a duplicated subtree. Its escape check applies to it
// like any other link. Refs: SEC-03
func TestBuild_ADirectorySymlink_IsDeliveredAsALinkNotACopy(t *testing.T) {
	wt := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(wt, "pkg", "core"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(wt, "pkg", "core", "x.go"), []byte("x"), 0o600))
	require.NoError(t, os.Symlink("pkg/core", filepath.Join(wt, "core-link")))

	stage := filepath.Join(t.TempDir(), "stage")
	require.NoError(t, Build(wt, privateStoreWith(t), stage))

	fi, err := os.Lstat(filepath.Join(stage, "core-link"))
	require.NoError(t, err)
	assert.NotZero(t, fi.Mode()&os.ModeSymlink,
		"a directory link stays a link: materializing it would duplicate the subtree "+
			"and break the aliasing the repo declared")
	// Reading THROUGH it must still find the target — the link is only useful
	// if the tree it points at was delivered beside it.
	assert.FileExists(t, filepath.Join(stage, "pkg", "core", "x.go"))
	assert.Equal(t, "pkg/core", readLink(t, filepath.Join(stage, "core-link")))
}

// writeIn creates a file in dir and returns its absolute path.
func writeIn(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(p, []byte("content of "+name), 0o600))
	return p
}

// readLink returns a link's target text.
func readLink(t *testing.T, path string) string {
	t.Helper()
	target, err := os.Readlink(path)
	require.NoError(t, err)
	return target
}
