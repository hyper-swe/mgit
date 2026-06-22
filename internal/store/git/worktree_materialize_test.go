package git

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// TestMaterializeBranchTo_WritesBranchTreeIntoArbitraryDir is the MGIT-17
// regression: `mgit worktree add` must materialize a branch's source tree into
// the LINKED worktree path (a separate FS root), not leave it empty. It also
// preserves file mode (executable bit) and never touches the project's .git.
// Refs: FR-16, MGIT-17
func TestMaterializeBranchTo_WritesBranchTreeIntoArbitraryDir(t *testing.T) {
	repo := initTestRepo(t)
	cs := NewCommitStore(repo)
	bs := NewBranchStore(repo)
	ws := NewWorktreeStore(repo)
	ctx := context.Background()
	root := repo.Root()

	// Commit a regular file, a nested file, and an executable script to HEAD.
	require.NoError(t, os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "pkg"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(root, "pkg", "lib.go"), []byte("package pkg\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "build.sh"), []byte("#!/bin/sh\n"), 0o700)) //nolint:gosec // executable bit is the property under test
	require.NoError(t, ws.Add(ctx, "."))
	c := makeTestModelCommit(t, "MGIT-17")
	c.FileDiffs = nil
	head, err := cs.CreateCommit(ctx, c)
	require.NoError(t, err)

	// Branch the task at HEAD, then materialize it into a SEPARATE worktree dir.
	require.NoError(t, bs.CreateBranch(ctx, &model.Branch{Name: "task/MGIT-17", HeadCommit: head}))
	dest := filepath.Join(t.TempDir(), "linked-wt")

	require.NoError(t, ws.MaterializeBranchTo(ctx, "task/MGIT-17", dest))

	// The worktree dir now holds the branch's source, mode preserved.
	got, err := os.ReadFile(filepath.Join(dest, "main.go")) //nolint:gosec // test-controlled path under t.TempDir()
	require.NoError(t, err)
	assert.Equal(t, "package main\n", string(got))
	got, err = os.ReadFile(filepath.Join(dest, "pkg", "lib.go")) //nolint:gosec // test-controlled path under t.TempDir()
	require.NoError(t, err)
	assert.Equal(t, "package pkg\n", string(got))

	info, err := os.Stat(filepath.Join(dest, "build.sh"))
	require.NoError(t, err)
	assert.NotZero(t, info.Mode().Perm()&0o100, "executable bit must be materialized")

	// The materialized worktree must NOT contain a .git or .mgit (mgit never
	// commits them, and materialize must not fabricate them).
	_, err = os.Stat(filepath.Join(dest, ".git"))
	assert.True(t, os.IsNotExist(err), "materialize must not create a .git in the worktree")
}
