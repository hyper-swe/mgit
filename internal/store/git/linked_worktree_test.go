package git

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// TestOpenLinked_CommitAdvancesBoundBranch_NotSharedHead is the core of the
// linked-worktree model (ADR-007): a commit made through a Repository opened via
// OpenLinked advances the worktree's BOUND branch and shares the parent's object
// store, while the parent's HEAD/main ref is never touched. Refs: FR-16, MGIT-24
func TestOpenLinked_CommitAdvancesBoundBranch_NotSharedHead(t *testing.T) {
	parent := initTestRepo(t)
	bs := NewBranchStore(parent)
	ctx := context.Background()

	// Parent main gets a file; bind a task branch at that tip.
	mainTip := createCommitWithFile(t, parent, "main.go", "package main\n", "MGIT-1.1")
	require.NoError(t, bs.CreateBranch(ctx, &model.Branch{Name: "task/MGIT-1", HeadCommit: mainTip}))
	mainBefore, err := parent.Head()
	require.NoError(t, err)
	require.Equal(t, mainTip, mainBefore)

	// Open a separate worktree dir LINKED to the parent store, bound to the task.
	// The worktree has its own .mgit dir (per-worktree staging lives there; the
	// real `worktree add` creates it for the marker + adapter shims).
	wtRoot := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(wtRoot, ".mgit"), 0o750))
	parentMgit := filepath.Join(parent.Root(), ".mgit")
	linked, err := OpenLinked(wtRoot, parentMgit, "task/MGIT-1", fixedClock())
	require.NoError(t, err)
	t.Cleanup(func() { _ = linked.Close() })

	cur, err := linked.CurrentBranch()
	require.NoError(t, err)
	assert.Equal(t, "task/MGIT-1", cur, "linked repo's current branch is the bound branch")

	// Commit a new file from the worktree root.
	require.NoError(t, os.WriteFile(filepath.Join(wtRoot, "feat.go"), []byte("package feat\n"), 0o600))
	require.NoError(t, NewWorktreeStore(linked).Add(ctx, "feat.go"))
	lc := makeTestModelCommit(t, "MGIT-1")
	lc.FileDiffs = nil
	lc.Message = "[MGIT:MGIT-1] add feat"
	newHash, err := NewCommitStore(linked).CreateCommit(ctx, lc)
	require.NoError(t, err)

	// The bound task branch advanced to the new commit...
	taskRef, err := parent.repo.Storer.Reference(plumbing.NewBranchReferenceName("task/MGIT-1"))
	require.NoError(t, err)
	assert.Equal(t, newHash, taskRef.Hash().String(), "commit must advance the bound task branch")

	// ...while the parent's main/HEAD is untouched.
	mainAfter, err := parent.Head()
	require.NoError(t, err)
	assert.Equal(t, mainBefore, mainAfter, "a worktree commit must not move the parent's HEAD/main")

	// The object is in the shared store and carries the worktree's file.
	got, err := NewCommitStore(parent).GetFileFromCommit(ctx, newHash, "feat.go")
	require.NoError(t, err)
	assert.Equal(t, "package feat\n", string(got))
}

// TestOpenLinked_EmptyBranch_Rejected guards the constructor contract.
func TestOpenLinked_EmptyBranch_Rejected(t *testing.T) {
	parent := initTestRepo(t)
	_, err := OpenLinked(t.TempDir(), filepath.Join(parent.Root(), ".mgit"), "", fixedClock())
	assert.Error(t, err, "an empty bound branch must be rejected")
}
