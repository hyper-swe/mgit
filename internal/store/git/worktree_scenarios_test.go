package git

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// branchModel is a small constructor for a *model.Branch in tests.
func branchModel(name, head string) *model.Branch {
	return &model.Branch{Name: name, HeadCommit: head}
}

// commitAll stages everything and commits under taskID, returning the hash.
func commitAll(t *testing.T, repo *Repository, taskID string) string {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, NewWorktreeStore(repo).Add(ctx, "."))
	c := makeTestModelCommit(t, taskID)
	c.FileDiffs = nil
	h, err := NewCommitStore(repo).CreateCommit(ctx, c)
	require.NoError(t, err)
	return h
}

// TestWorktree_SymlinkRoundTrip_PreservedThroughCommitAndMaterialize: a symlink
// is committed as link text (mode 120000) and recreated as a real symlink when
// the branch is materialized into a worktree. Refs: MGIT-14.7
func TestWorktree_SymlinkRoundTrip_PreservedThroughCommitAndMaterialize(t *testing.T) {
	repo := initTestRepo(t)
	bs := NewBranchStore(repo)
	ctx := context.Background()
	root := repo.Root()

	require.NoError(t, os.WriteFile(filepath.Join(root, "target.txt"), []byte("hi\n"), 0o600))
	require.NoError(t, os.Symlink("target.txt", filepath.Join(root, "link.txt")))
	head := commitAll(t, repo, "MGIT-1")

	require.NoError(t, bs.CreateBranch(ctx, branchModel("task/MGIT-1", head)))
	dest := filepath.Join(t.TempDir(), "wt")
	require.NoError(t, NewWorktreeStore(repo).MaterializeBranchTo(ctx, "task/MGIT-1", dest))

	info, err := os.Lstat(filepath.Join(dest, "link.txt"))
	require.NoError(t, err)
	assert.NotZero(t, info.Mode()&os.ModeSymlink, "materialized link.txt must be a symlink")
	tgt, err := os.Readlink(filepath.Join(dest, "link.txt"))
	require.NoError(t, err)
	assert.Equal(t, "target.txt", tgt)
}

// TestWorktreeStore_Status_ModifiedDeletedStaged exercises the status classifier
// across modified, deleted, and staged-deletion outcomes.
func TestWorktreeStore_Status_ModifiedDeletedStaged(t *testing.T) {
	repo := initTestRepo(t)
	ws := NewWorktreeStore(repo)
	ctx := context.Background()
	root := repo.Root()

	require.NoError(t, os.WriteFile(filepath.Join(root, "keep.go"), []byte("v1\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "gone.go"), []byte("x\n"), 0o600))
	commitAll(t, repo, "MGIT-1")

	// Modify keep.go, delete gone.go.
	require.NoError(t, os.WriteFile(filepath.Join(root, "keep.go"), []byte("v2 changed\n"), 0o600))
	require.NoError(t, os.Remove(filepath.Join(root, "gone.go")))

	st, err := ws.Status(ctx)
	require.NoError(t, err)
	byPath := map[string]FileStatus{}
	for _, f := range st {
		byPath[f.Path] = f
	}
	assert.Equal(t, statusModified, byPath["keep.go"].Worktree, "modified tracked file is M")
	assert.Equal(t, statusDeleted, byPath["gone.go"].Worktree, "removed tracked file is D")

	// Stage the deletion -> staging shows D.
	require.NoError(t, ws.Add(ctx, "gone.go"))
	st, err = ws.Status(ctx)
	require.NoError(t, err)
	for _, f := range st {
		if f.Path == "gone.go" {
			assert.Equal(t, statusDeleted, f.Staging, "staged deletion shows D in staging")
		}
	}
}

// TestWorktreeStore_Add_InvalidPath_Rejected: a traversal path is rejected.
func TestWorktreeStore_Add_InvalidPath_Rejected(t *testing.T) {
	repo := initTestRepo(t)
	err := NewWorktreeStore(repo).Add(context.Background(), "../escape.go")
	assert.Error(t, err, "a path escaping the worktree must be rejected")
}

// TestWorktreeStore_Add_NonexistentUntracked_Rejected: staging a path that is
// neither on disk nor tracked fails like `git add` of a missing pathspec.
func TestWorktreeStore_Add_NonexistentUntracked_Rejected(t *testing.T) {
	repo := initTestRepo(t)
	err := NewWorktreeStore(repo).Add(context.Background(), "nope.go")
	assert.Error(t, err, "an unmatched pathspec must be rejected")
}

// TestWorktreeStore_Checkout_NonexistentBranch_Error.
func TestWorktreeStore_Checkout_NonexistentBranch_Error(t *testing.T) {
	repo := initTestRepo(t)
	err := NewWorktreeStore(repo).Checkout(context.Background(), "no-such-branch")
	assert.ErrorIs(t, err, model.ErrBranchNotFound)
}

// TestWorktreeStore_Checkout_DirtyTracked_Refused: a checkout that would clobber
// an uncommitted change to a tracked file is refused.
func TestWorktreeStore_Checkout_DirtyTracked_Refused(t *testing.T) {
	repo := initTestRepo(t)
	bs := NewBranchStore(repo)
	ws := NewWorktreeStore(repo)
	ctx := context.Background()
	root := repo.Root()

	require.NoError(t, os.WriteFile(filepath.Join(root, "f.go"), []byte("v1\n"), 0o600))
	head := commitAll(t, repo, "MGIT-1")
	require.NoError(t, bs.CreateBranch(ctx, branchModel("task/MGIT-1", head)))

	// Uncommitted modification to a tracked file.
	require.NoError(t, os.WriteFile(filepath.Join(root, "f.go"), []byte("DIRTY\n"), 0o600))
	err := ws.Checkout(ctx, "task/MGIT-1")
	assert.ErrorIs(t, err, model.ErrRollbackConflict, "a dirty tracked tree blocks checkout")
}

// TestWorktreeStore_Clean_RemovesUntracked: clean deletes untracked files only.
func TestWorktreeStore_Clean_RemovesUntracked(t *testing.T) {
	repo := initTestRepo(t)
	ws := NewWorktreeStore(repo)
	ctx := context.Background()
	root := repo.Root()

	require.NoError(t, os.WriteFile(filepath.Join(root, "tracked.go"), []byte("t\n"), 0o600))
	commitAll(t, repo, "MGIT-1")
	require.NoError(t, os.WriteFile(filepath.Join(root, "junk.txt"), []byte("junk\n"), 0o600))

	require.NoError(t, ws.Clean(ctx))
	_, err := os.Stat(filepath.Join(root, "junk.txt"))
	assert.True(t, os.IsNotExist(err), "untracked file removed")
	_, err = os.Stat(filepath.Join(root, "tracked.go"))
	assert.NoError(t, err, "tracked file kept")
}

// TestWriteEntryToDir_InvalidPath_Rejected: a tree entry whose path escapes the
// destination root is rejected before any write.
func TestWriteEntryToDir_InvalidPath_Rejected(t *testing.T) {
	repo := initTestRepo(t)
	err := repo.writeEntryToDir(t.TempDir(), "../escape", blobEntry{mode: filemode.Regular})
	assert.Error(t, err, "an escaping entry path must be rejected")
}
