package staging

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rootUnderSymlinkedPrefix returns two paths to the SAME worktree directory:
// one reached directly and one reached through a symlinked parent — the
// /tmp -> /private/tmp shape macOS gives every `mgit work /tmp/...` worktree.
//
// It is built explicitly rather than taken from the host's layout, so the
// two cases stay separate on every platform: on macOS t.TempDir itself sits
// under /var -> /private/var, which would make the "direct" path a symlinked
// one too and collapse the comparison this helper exists for.
func rootUnderSymlinkedPrefix(t *testing.T) (direct, viaPrefix string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	realParent := filepath.Join(base, "real")
	direct = filepath.Join(realParent, "wt")
	require.NoError(t, os.MkdirAll(direct, 0o750))
	require.NoError(t, os.Symlink(realParent, filepath.Join(base, "prefix")))
	viaPrefix = filepath.Join(base, "prefix", "wt")
	return direct, viaPrefix
}

// TestAssertSymlinkWithin_DanglingInRootLink_UnderASymlinkedPrefix pins the
// half of the guard's contract that a symlinked prefix broke:
//
//	A link to a not-yet-existing in-root path is allowed; one pointing
//	outside root (absolute or via ..) is rejected.
//
// The root was canonicalised and an EXISTING target was; a target that did
// not exist yet was compared lexically, so under /var -> /private/var the
// safe link read as `../../..`-prefixed and was refused as an escape — with
// a containment message about a breach that did not exist, on the exact path
// CLAUDE.md tells every agent to use. Every row runs twice, under a direct
// root and under a symlinked prefix; the verdict must not depend on which.
// The rows come from the doc comment and SEC-03, not from the code.
// Refs: MGIT-166, SEC-03, F-A/NEW-2
func TestAssertSymlinkWithin_DanglingInRootLink_UnderASymlinkedPrefix(t *testing.T) {
	tests := []struct {
		name string
		// target returns the link text, given the root the link lives in
		// and the directory the link is placed in.
		target     func(t *testing.T, root string) string
		linkDir    string // relative to root; "" is the root itself
		wantEscape bool
	}{
		{
			name:   "dangling_link_to_an_in_root_leaf_is_allowed",
			target: func(*testing.T, string) string { return "generated-later.txt" },
		},
		{
			name:    "dangling_link_from_a_subdirectory_to_an_in_root_leaf_is_allowed",
			target:  func(*testing.T, string) string { return filepath.Join("..", "dist", "bundle.js") },
			linkDir: "src",
		},
		{
			name: "dangling_leaf_under_an_existing_in_root_directory_is_allowed",
			target: func(t *testing.T, root string) string {
				require.NoError(t, os.MkdirAll(filepath.Join(root, "build"), 0o750))
				return filepath.Join("build", "out", "not-yet")
			},
		},
		{
			name: "existing_in_root_target_is_allowed",
			target: func(t *testing.T, root string) string {
				require.NoError(t, os.WriteFile(filepath.Join(root, "real.txt"), []byte("x"), 0o600))
				return "real.txt"
			},
		},
		{
			name:       "absolute_target_outside_the_root_is_rejected",
			target:     func(t *testing.T, _ string) string { return filepath.Join(t.TempDir(), "secret") },
			wantEscape: true,
		},
		{
			name: "dotdot_target_that_leaves_the_root_is_rejected",
			target: func(t *testing.T, root string) string {
				rel, err := filepath.Rel(root, filepath.Join(t.TempDir(), "secret"))
				require.NoError(t, err)
				require.True(t, strings.HasPrefix(rel, ".."+string(filepath.Separator)), "must genuinely leave the root: %q", rel)
				return rel
			},
			wantEscape: true,
		},
		{
			name:       "dangling_dotdot_chain_that_leaves_the_root_is_rejected",
			target:     func(*testing.T, string) string { return filepath.Join("missing", "..", "..", "..", "etc", "passwd") },
			wantEscape: true,
		},
		{
			name: "dangling_leaf_under_an_in_root_directory_link_that_points_outside_is_rejected",
			target: func(t *testing.T, root string) string {
				outsideDir := t.TempDir()
				require.NoError(t, os.Symlink(outsideDir, filepath.Join(root, "hop")))
				return filepath.Join("hop", "not-yet.txt")
			},
			wantEscape: true,
		},
		{
			name: "chain_of_links_cannot_step_outside_one_hop_at_a_time",
			target: func(t *testing.T, root string) string {
				outside := filepath.Join(t.TempDir(), "secret")
				require.NoError(t, os.WriteFile(outside, []byte("x"), 0o600))
				require.NoError(t, os.Symlink(outside, filepath.Join(root, "hop")))
				return "hop"
			},
			wantEscape: true,
		},
	}
	for _, tt := range tests {
		for _, via := range []struct {
			name     string
			prefixed bool
		}{{"direct_root", false}, {"root_under_a_symlinked_prefix", true}} {
			t.Run(tt.name+"/"+via.name, func(t *testing.T) {
				direct, viaPrefix := rootUnderSymlinkedPrefix(t)
				root := direct
				if via.prefixed {
					root = viaPrefix
				}
				linkDir := root
				if tt.linkDir != "" {
					linkDir = filepath.Join(root, tt.linkDir)
					require.NoError(t, os.MkdirAll(linkDir, 0o750))
				}
				link := filepath.Join(linkDir, "l")
				require.NoError(t, os.Symlink(tt.target(t, root), link))

				err := AssertSymlinkWithin(root, link)

				if tt.wantEscape {
					require.ErrorIs(t, err, ErrSymlinkEscape)
					return
				}
				assert.NoError(t, err, "a safe link must not be refused, and never with a containment message")
			})
		}
	}
}

// TestBuild_ADanglingInRootLink_UnderASymlinkedPrefix_IsDelivered is the
// whole verb: the refusal fails the entire launch or sync closed, so a repo
// with a link to a not-yet-built path could be launched under ~/ and not
// under /tmp. Refs: MGIT-166, SEC-03
func TestBuild_ADanglingInRootLink_UnderASymlinkedPrefix_IsDelivered(t *testing.T) {
	_, wt := rootUnderSymlinkedPrefix(t)
	require.NoError(t, os.WriteFile(filepath.Join(wt, "src.txt"), []byte("src"), 0o600))
	require.NoError(t, os.Symlink(filepath.Join("build", "out"), filepath.Join(wt, "dist")))
	staged := t.TempDir()

	err := Build(wt, privateStoreWith(t), staged)

	require.NoError(t, err)
	got, err := os.Readlink(filepath.Join(staged, "dist"))
	require.NoError(t, err, "the link is delivered")
	assert.Equal(t, filepath.Join("build", "out"), got, "with its target text intact")
	src, err := os.ReadFile(filepath.Join(staged, "src.txt")) //nolint:gosec // a t.TempDir path this test wrote
	require.NoError(t, err)
	assert.Equal(t, "src", string(src), "and the rest of the tree is delivered around it")
}
