package git

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A linked worktree materialized INSIDE the repo root used to be walked as
// project content: hashed by every later working-tree fingerprint (quadratic as
// a worker pool warms up) and absorbed into the shared base by the auto-resync,
// so every new worktree materialized a copy of every earlier one. The walk now
// skips any nested mgit root wherever it sits. Refs: MGIT-120, FR-16, ADR-008 §2

// TestListWorkingFiles_NestedWorktree_IsNotProjectContent proves the walk skips
// a linked worktree inside the repo root while still listing real project files
// — including one whose name merely starts like the worktree's.
func TestListWorkingFiles_NestedWorktree_IsNotProjectContent(t *testing.T) {
	repo := initTestRepo(t)
	ws := NewWorktreeStore(repo)

	writeFileMk(t, repo.Root(), "src/main.go", "package main\n")
	writeFileMk(t, repo.Root(), "wt-taskish.txt", "a normal file, not a worktree\n")
	// A materialized worktree: content plus its own .mgit marker.
	wtRoot := filepath.Join(repo.Root(), "wt-T-1")
	writeFileMk(t, wtRoot, "src/main.go", "package main\n")
	writeFileMk(t, wtRoot, "big/blob.txt", "a whole second copy of the project\n")
	require.NoError(t, ws.WriteWorktreeMarker(wtRoot, "task/T-1", "T-1"))

	paths, err := repo.listWorkingFiles()
	require.NoError(t, err)

	assert.Contains(t, paths, "src/main.go")
	assert.Contains(t, paths, "wt-taskish.txt", "a file that merely looks worktree-ish is project content")
	for _, p := range paths {
		assert.NotContains(t, p, "wt-T-1/", "a nested worktree must never be walked as project content: %s", p)
	}
}

// TestListWorkingFiles_NestedMgitDirWithoutMarker_IsSkipped covers the race the
// unlocked materialization opens: a peer's `mgit work` creates the worktree's
// .mgit before it has written the marker or the content. Keying the skip on the
// .mgit directory (not the marker file) means a half-provisioned worktree is
// never absorbed either. Refs: MGIT-120
func TestListWorkingFiles_NestedMgitDirWithoutMarker_IsSkipped(t *testing.T) {
	repo := initTestRepo(t)

	writeFileMk(t, repo.Root(), "keep.txt", "project content\n")
	writeFileMk(t, repo.Root(), "wt-inflight/.mgit/scratch", "")
	writeFileMk(t, repo.Root(), "wt-inflight/src/partial.go", "package partial\n")

	paths, err := repo.listWorkingFiles()
	require.NoError(t, err)

	assert.Contains(t, paths, "keep.txt")
	for _, p := range paths {
		assert.NotContains(t, p, "wt-inflight/", "an in-flight worktree must not be walked: %s", p)
	}
}

// TestAddAll_NestedWorktree_IsNotStaged is the consequence that matters: the
// auto-resync stages the working tree, so a walked worktree would land IN the
// shared base. Refs: MGIT-120, ADR-008 §2
func TestAddAll_NestedWorktree_IsNotStaged(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()
	ws := NewWorktreeStore(repo)

	writeFileMk(t, repo.Root(), "a.go", "package a\n")
	wtRoot := filepath.Join(repo.Root(), "wt-T-9")
	writeFileMk(t, wtRoot, "a.go", "package a\n")
	require.NoError(t, ws.WriteWorktreeMarker(wtRoot, "task/T-9", "T-9"))

	require.NoError(t, ws.Add(ctx, "."))
	staged, err := repo.stagedPaths()
	require.NoError(t, err)
	assert.Equal(t, []string{"a.go"}, staged,
		"another task's worktree must never be absorbed into the shared base")
}
