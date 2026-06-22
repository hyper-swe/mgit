package git

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// stageCommitFile writes file with content to the repo root, stages it, and
// creates a task-tagged commit via the store, returning its hash. It produces a
// real, differing git tree per commit so squash's tree-diff isolation is
// exercised at the store layer.
func stageCommitFile(t *testing.T, repo *Repository, cs *CommitStore, task, file, content string) string {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, os.WriteFile(filepath.Join(repo.Root(), file), []byte(content), 0o600))
	require.NoError(t, NewWorktreeStore(repo).Add(ctx, file))
	c := makeTestModelCommit(t, task)
	c.FileDiffs = nil
	hash, err := cs.CreateCommit(ctx, c)
	require.NoError(t, err)
	return hash
}

// TestCreateSquashCommit_IsolatesTask_OffBase_WithoutAdvancingHead verifies the
// store primitive: the squash captures only the task's net changes, parents off
// the task's base, lands on its own branch ref, and never advances HEAD or
// touches the originals. Refs: FR-7, FR-12, MGIT-22
func TestCreateSquashCommit_IsolatesTask_OffBase_WithoutAdvancingHead(t *testing.T) {
	repo := initTestRepo(t)
	cs := NewCommitStore(repo)
	ctx := context.Background()

	hA := stageCommitFile(t, repo, cs, "MGIT-1", "a.go", "package a\n")
	hB := stageCommitFile(t, repo, cs, "MGIT-1", "b.go", "package b\n")
	stageCommitFile(t, repo, cs, "MGIT-2", "c.go", "package c\n") // unrelated tip

	headBefore, err := repo.Head()
	require.NoError(t, err)

	baseObj, err := repo.repo.CommitObject(plumbing.NewHash(hA))
	require.NoError(t, err)
	wantBase := baseObj.ParentHashes[0].String()

	squash := makeTestModelCommit(t, "MGIT-1")
	squash.FileDiffs = nil
	squash.CommitType = model.CommitTypeSquash
	hash, err := cs.CreateSquashCommit(ctx, SquashCommitParams{
		Commit:      squash,
		TaskCommits: []string{hA, hB},
		Branch:      "task/MGIT-1",
	})
	require.NoError(t, err)

	// HEAD/integration branch is untouched.
	headAfter, err := repo.Head()
	require.NoError(t, err)
	assert.Equal(t, headBefore, headAfter, "squash must not advance HEAD")

	// Parented off the task base, not the unrelated MGIT-2 tip.
	assert.Equal(t, wantBase, squash.ParentID, "squash parent must be the task base")

	// task/MGIT-1 ref points at the squash.
	ref, err := repo.repo.Storer.Reference(plumbing.NewBranchReferenceName("task/MGIT-1"))
	require.NoError(t, err)
	assert.Equal(t, hash, ref.Hash().String())

	// Tree contains only the task's files.
	a, err := cs.GetFileFromCommit(ctx, hash, "a.go")
	require.NoError(t, err)
	assert.Equal(t, "package a\n", string(a))
	b, err := cs.GetFileFromCommit(ctx, hash, "b.go")
	require.NoError(t, err)
	assert.Equal(t, "package b\n", string(b))
	_, err = cs.GetFileFromCommit(ctx, hash, "c.go")
	assert.ErrorIs(t, err, model.ErrFileNotFound, "unrelated task's file must be absent")
}

// TestCreateSquashCommit_RootBase_ProducesParentlessSquash covers the edge where
// the task's first commit is itself parentless (a root): the squash is built on
// an empty base and is itself parentless. Refs: MGIT-22
func TestCreateSquashCommit_RootBase_ProducesParentlessSquash(t *testing.T) {
	repo := initTestRepo(t)
	cs := NewCommitStore(repo)
	ctx := context.Background()

	// mgit's own initial commit is the only parentless (root) commit.
	genesis, err := repo.Head()
	require.NoError(t, err)
	require.NotEmpty(t, genesis)

	squash := makeTestModelCommit(t, "MGIT-1")
	squash.FileDiffs = nil
	squash.CommitType = model.CommitTypeSquash
	hash, err := cs.CreateSquashCommit(ctx, SquashCommitParams{
		Commit:      squash,
		TaskCommits: []string{genesis},
		Branch:      "task/MGIT-1",
	})
	require.NoError(t, err)
	assert.NotEmpty(t, hash)
	assert.Empty(t, squash.ParentID, "squash off a root base must itself be parentless")
}

// TestCreateSquashCommit_TaskDeletesBaseFile_OmittedFromSquash covers the
// deletion path: when the task removes a file that existed in its base tree,
// the squash tree must omit it. Refs: MGIT-22
func TestCreateSquashCommit_TaskDeletesBaseFile_OmittedFromSquash(t *testing.T) {
	repo := initTestRepo(t)
	cs := NewCommitStore(repo)
	ctx := context.Background()

	// A pre-existing file lands before the task starts, so it is in the base.
	stageCommitFile(t, repo, cs, "MGIT-0", "keep.go", "package keep\n")

	// The task's first commit deletes that file (stage the path with it removed
	// from disk → buildTreeFromStaging records a deletion).
	root := repo.Root()
	require.NoError(t, os.Remove(filepath.Join(root, "keep.go")))
	require.NoError(t, NewWorktreeStore(repo).Add(ctx, "keep.go"))
	del := makeTestModelCommit(t, "MGIT-3")
	del.FileDiffs = nil
	hDel, err := cs.CreateCommit(ctx, del)
	require.NoError(t, err)

	squash := makeTestModelCommit(t, "MGIT-3")
	squash.FileDiffs = nil
	squash.CommitType = model.CommitTypeSquash
	hash, err := cs.CreateSquashCommit(ctx, SquashCommitParams{
		Commit:      squash,
		TaskCommits: []string{hDel},
		Branch:      "task/MGIT-3",
	})
	require.NoError(t, err)

	_, err = cs.GetFileFromCommit(ctx, hash, "keep.go")
	assert.ErrorIs(t, err, model.ErrFileNotFound,
		"a base file the task deleted must be omitted from the squash tree")
}

// TestCreateSquashCommit_TaskChangesMode_PreservedInSquash covers a mode-only
// change: when the task flips a base file's executable bit without editing its
// bytes, the squash tree must carry the NEW mode (the blob hash is unchanged,
// so a hash-only diff would drop it). Refs: MGIT-22
func TestCreateSquashCommit_TaskChangesMode_PreservedInSquash(t *testing.T) {
	repo := initTestRepo(t)
	cs := NewCommitStore(repo)
	ctx := context.Background()

	// A non-executable script lands before the task starts (in the base).
	stageCommitFile(t, repo, cs, "MGIT-0", "build.sh", "#!/bin/sh\necho hi\n")

	// The task's only change: chmod +x — identical bytes, new mode.
	scriptPath := filepath.Join(repo.Root(), "build.sh")
	require.NoError(t, os.Chmod(scriptPath, 0o755)) //nolint:gosec // the executable bit IS the change under test
	require.NoError(t, NewWorktreeStore(repo).Add(ctx, "build.sh"))
	chmod := makeTestModelCommit(t, "MGIT-3")
	chmod.FileDiffs = nil
	hChmod, err := cs.CreateCommit(ctx, chmod)
	require.NoError(t, err)

	squash := makeTestModelCommit(t, "MGIT-3")
	squash.FileDiffs = nil
	squash.CommitType = model.CommitTypeSquash
	hash, err := cs.CreateSquashCommit(ctx, SquashCommitParams{
		Commit:      squash,
		TaskCommits: []string{hChmod},
		Branch:      "task/MGIT-3",
	})
	require.NoError(t, err)

	// The squash tree's build.sh entry must be executable (100755), not the
	// base's 100644.
	commitObj, err := repo.repo.CommitObject(plumbing.NewHash(hash))
	require.NoError(t, err)
	tree, err := commitObj.Tree()
	require.NoError(t, err)
	entry, err := tree.FindEntry("build.sh")
	require.NoError(t, err)
	assert.Equal(t, filemode.Executable, entry.Mode,
		"a task's mode-only change must survive the squash")
}

// TestCreateSquashCommit_Validation rejects empty inputs.
func TestCreateSquashCommit_Validation(t *testing.T) {
	repo := initTestRepo(t)
	cs := NewCommitStore(repo)
	ctx := context.Background()
	head, err := repo.Head()
	require.NoError(t, err)

	_, err = cs.CreateSquashCommit(ctx, SquashCommitParams{
		Commit: makeTestModelCommit(t, "MGIT-1"), TaskCommits: nil, Branch: "task/MGIT-1",
	})
	assert.ErrorIs(t, err, model.ErrTaskNotFound, "no commits must be rejected")

	_, err = cs.CreateSquashCommit(ctx, SquashCommitParams{
		Commit: makeTestModelCommit(t, "MGIT-1"), TaskCommits: []string{head}, Branch: "",
	})
	assert.Error(t, err, "empty branch must be rejected")
}
