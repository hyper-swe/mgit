package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gitstore "github.com/hyper-swe/mgit/internal/store/git"
)

// sandboxRepoRoot decides which daemon a sandbox verb addresses. FEAT-7.28 /
// MGIT-196: `mgit sandbox launch --worktree <dir>` writes agent files under
// <dir>/.mgit, after which <dir> resolved as a repository of its own — its own
// daemon, an empty registry, and status/run/doctor answering three different
// things about one sandbox. Each shape below is laid out by hand, so the cases
// do not derive from the code under test. Refs: MGIT-196, MGIT-57
func TestSandboxRepoRoot_FollowsMarkerThenOwner_AndRefusesDecorationAlone(t *testing.T) {
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	require.NoError(t, os.MkdirAll(repo, 0o750))
	require.NoError(t, runCLI(t, "init", "--path", repo))

	linked := filepath.Join(base, "linked")
	require.NoError(t, os.MkdirAll(filepath.Join(linked, ".mgit"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(linked, ".mgit", "worktree"),
		[]byte(`{"store":"`+filepath.Join(repo, ".mgit")+`","branch":"task/T-1","task":"T-1"}`), 0o600))

	owned := filepath.Join(base, "owned")
	require.NoError(t, gitstore.WriteSandboxOwner(owned, gitstore.SandboxOwner{RepoRoot: repo, Task: "T-2"}))
	deep := filepath.Join(owned, "pkg", "deep")
	require.NoError(t, os.MkdirAll(deep, 0o750))

	decorated := filepath.Join(base, "decorated")
	require.NoError(t, os.MkdirAll(filepath.Join(decorated, ".mgit", "shims"), 0o750))

	bare := filepath.Join(base, "bare")
	require.NoError(t, os.MkdirAll(filepath.Join(bare, ".mgit"), 0o750))

	gone := filepath.Join(base, "gone")
	orphan := filepath.Join(base, "orphan")
	require.NoError(t, gitstore.WriteSandboxOwner(orphan, gitstore.SandboxOwner{RepoRoot: gone, Task: "T-3"}))

	tests := []struct {
		name    string
		start   string
		want    string
		wantErr []string
	}{
		{name: "a_repository_root_is_its_own", start: repo, want: repo},
		{name: "a_linked_worktree_resolves_to_its_marker's_store", start: linked, want: repo},
		{name: "a_launch_decorated_dir_resolves_to_its_recorded_owner", start: owned, want: repo},
		{name: "from_a_subdirectory_of_the_decorated_dir_as_well", start: deep, want: repo},
		{name: "a_bare_mgit_with_no_decoration_is_a_root_as_before", start: bare, want: bare},
		{
			name: "decoration_without_an_owner_is_refused_naming_the_remedy", start: decorated,
			wantErr: []string{decorated + "/.mgit holds no store", "mgit sandbox launch"},
		},
		{
			name: "an_owner_that_is_not_a_repository_is_refused_naming_both", start: orphan,
			wantErr: []string{gone, "holds no mgit store", "T-3"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := sandboxRepoRoot(tt.start)
			if len(tt.wantErr) > 0 {
				require.Error(t, err)
				for _, w := range tt.wantErr {
					assert.Contains(t, err.Error(), w)
				}
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// One repository reached through three spellings of its path — direct, a
// symlinked prefix (/tmp is one on macOS), a symlinked leaf — got three
// daemons over one index, whose registries diverged and whose rehydrations
// deleted each other's live sandboxes (MGIT-197). The daemon key is the
// repository, not the spelling: every spelling resolves to one root.
// Refs: MGIT-197
func TestSandboxRepoRoot_SpellingsOfOneRepositoryResolveToOneRoot(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	require.NoError(t, os.MkdirAll(real, 0o750))
	require.NoError(t, runCLI(t, "init", "--path", real))
	prefix := filepath.Join(t.TempDir(), "prefix")
	require.NoError(t, os.Symlink(base, prefix))
	leaf := filepath.Join(t.TempDir(), "leaf")
	require.NoError(t, os.Symlink(real, leaf))
	// A linked worktree whose marker spells the store through the symlinked
	// prefix, as `mgit work` run from that spelling would write it.
	linked := filepath.Join(base, "linked")
	require.NoError(t, os.MkdirAll(filepath.Join(linked, ".mgit"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(linked, ".mgit", "worktree"),
		[]byte(`{"store":"`+filepath.Join(prefix, "real", ".mgit")+`","branch":"task/T-1","task":"T-1"}`), 0o600))
	// A sandbox worktree whose owner file spells the root through the leaf.
	owned := filepath.Join(base, "owned")
	require.NoError(t, gitstore.WriteSandboxOwner(owned, gitstore.SandboxOwner{RepoRoot: leaf, Task: "T-2"}))

	want, err := resolveSandboxPaths(real)
	require.NoError(t, err)
	for _, start := range []string{real, filepath.Join(prefix, "real"), leaf, filepath.Join(leaf, "pkg"), linked, owned} {
		if filepath.Base(start) == "pkg" {
			require.NoError(t, os.MkdirAll(start, 0o750))
		}
		root, err := sandboxRepoRoot(start)
		require.NoError(t, err, start)
		got, err := resolveSandboxPaths(root)
		require.NoError(t, err)
		assert.Equal(t, want.socket, got.socket, "one daemon socket for every spelling (from %s, root %s)", start, root)
		// The paths handed to the daemon keep the spelling the caller used;
		// only the key is canonical.
		assert.Equal(t, filepath.Join(root, ".mgit", "sandbox"), got.hostRoot)
	}
}
