package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gitstore "github.com/hyper-swe/mgit/internal/store/git"
)

// TestSandboxRepoRoot_WorktreeResolvesSharedParent is the MGIT-57 regression:
// `mgit run` from inside a linked worktree must resolve the sandbox daemon of
// the SHARED PARENT repo, not treat the worktree (whose .mgit holds only the
// marker + shims) as a sandbox host — that spawned a second daemon pointed at
// a nonexistent host root, which died, so the README's own
// `work --sandbox` + `run` walkthrough failed on a real machine.
// Refs: MGIT-57, FR-17.34, NFR-17.6
func TestSandboxRepoRoot_WorktreeResolvesSharedParent(t *testing.T) {
	clock := func() time.Time { return time.Unix(0, 0).UTC() }
	parent := t.TempDir()
	repo, err := gitstore.Init(parent, clock)
	require.NoError(t, err)
	defer func() { _ = repo.Close() }()

	// A linked worktree: .mgit is a DIRECTORY carrying the marker (plus shims
	// in real life), exactly the layout that fooled the naive cwd check.
	wt := filepath.Join(parent, "wt")
	require.NoError(t, os.MkdirAll(wt, 0o750))
	require.NoError(t, gitstore.NewWorktreeStore(repo).WriteWorktreeMarker(wt, "task/MGIT-57", "MGIT-57"))

	tests := []struct {
		name string
		cwd  string
		want string
	}{
		{name: "repo_root_resolves_itself", cwd: parent, want: parent},
		{name: "worktree_resolves_shared_parent", cwd: wt, want: parent},
		{name: "worktree_subdir_resolves_shared_parent", cwd: filepath.Join(wt, "sub", "dir"), want: parent},
		{name: "repo_subdir_resolves_repo_root", cwd: filepath.Join(parent, "internal"), want: parent},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.NoError(t, os.MkdirAll(tt.cwd, 0o750))
			got, err := sandboxRepoRoot(tt.cwd)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestSandboxRepoRoot_NoRepo_Error: outside any repo the resolution fails
// with the standard not-a-repository error. Refs: MGIT-57
func TestSandboxRepoRoot_NoRepo_Error(t *testing.T) {
	_, err := sandboxRepoRoot(t.TempDir())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not an mgit repository")
}
