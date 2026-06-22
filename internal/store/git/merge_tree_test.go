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

// mergeCommitFiles loads the flattened file set (path -> blob+mode) of a commit
// hash, for asserting on a merge commit's resulting tree.
func mergeCommitFiles(t *testing.T, repo *Repository, hash string) map[string]blobEntry {
	t.Helper()
	obj, err := repo.repo.CommitObject(plumbing.NewHash(hash))
	require.NoError(t, err)
	tree, err := obj.Tree()
	require.NoError(t, err)
	files, err := flattenTree(tree)
	require.NoError(t, err)
	return files
}

// blobText returns the content of a blob hash as a string.
func blobText(t *testing.T, repo *Repository, h plumbing.Hash) string {
	t.Helper()
	data, err := repo.blobContent(h)
	require.NoError(t, err)
	return string(data)
}

// TestMergeStore_CreateMergeCommit_TreeIncorporatesSource verifies that a
// no-ff merge commit's tree REFLECTS THE MERGE: it contains the source
// branch's changes (a source-only file) layered on top of HEAD, not merely a
// reuse of HEAD's tree. This is the MGIT-15 bug: the old implementation reused
// HEAD's tree so source content never reached the merge commit.
// Refs: MGIT-15, FR-8.4
func TestMergeStore_CreateMergeCommit_TreeIncorporatesSource(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()
	bs := NewBranchStore(repo)
	ms := NewMergeStore(repo)

	// Base commit shared by both branches.
	baseHash := createCommitWithFile(t, repo, "base.go", "package base\n", "MGIT-1.1")

	// Feature branch diverges from base and adds a source-only file.
	tid, _ := model.ParseTaskID("MGIT-1.2")
	require.NoError(t, bs.CreateBranch(ctx, &model.Branch{Name: "feature", HeadCommit: baseHash, TaskID: tid}))
	require.NoError(t, bs.SwitchBranch(ctx, "feature"))
	featureHash := createCommitWithFile(t, repo, "feat_only.go", "package feat\n", "MGIT-1.3")

	// main advances with its own file.
	require.NoError(t, bs.SwitchBranch(ctx, "main"))
	mainHash := createCommitWithFile(t, repo, "main_only.go", "package main\n", "MGIT-1.4")

	mergeHash, err := ms.CreateMergeCommit(ctx, "merge feature into main", featureHash)
	require.NoError(t, err)

	files := mergeCommitFiles(t, repo, mergeHash)
	// The merge tree must contain BOTH sides' changes plus the base.
	assert.Contains(t, files, "base.go", "base file must survive the merge")
	assert.Contains(t, files, "main_only.go", "HEAD's own change must survive the merge")
	require.Contains(t, files, "feat_only.go", "source branch's change MUST be incorporated (MGIT-15 bug)")
	assert.Equal(t, "package feat\n", blobText(t, repo, files["feat_only.go"].hash))

	_ = mainHash
}

// TestMergeStore_CreateMergeCommit_NoFF_MaterializesWorkingTree verifies that a
// no-ff merge writes the merged tree onto disk so the working tree reflects the
// merge (source-only files appear with the source's content). Previously the
// working tree was left stale. Refs: MGIT-15
func TestMergeStore_CreateMergeCommit_NoFF_MaterializesWorkingTree(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()
	bs := NewBranchStore(repo)
	ms := NewMergeStore(repo)

	baseHash := createCommitWithFile(t, repo, "base.go", "package base\n", "MGIT-2.1")

	tid, _ := model.ParseTaskID("MGIT-2.2")
	require.NoError(t, bs.CreateBranch(ctx, &model.Branch{Name: "feature", HeadCommit: baseHash, TaskID: tid}))
	require.NoError(t, bs.SwitchBranch(ctx, "feature"))
	featureHash := createCommitWithFile(t, repo, "feat_only.go", "package feat\n", "MGIT-2.3")

	require.NoError(t, bs.SwitchBranch(ctx, "main"))
	createCommitWithFile(t, repo, "main_only.go", "package main\n", "MGIT-2.4")

	// Remove the source-only file from disk to prove materialization restores it.
	require.NoError(t, os.Remove(filepath.Join(repo.Root(), "feat_only.go")))

	_, err := ms.CreateMergeCommit(ctx, "merge feature into main", featureHash)
	require.NoError(t, err)

	got, err := os.ReadFile(filepath.Join(repo.Root(), "feat_only.go"))
	require.NoError(t, err, "merge must materialize the source-only file onto disk")
	assert.Equal(t, "package feat\n", string(got))
}

// TestMergeStore_CreateMergeCommit_NoFF_DeletesRemovedFileFromDisk verifies the
// deletion pass: a file the source branch deleted (and HEAD left untouched) is
// both absent from the merge commit's tree AND removed from disk. The deletion
// pass must key off the PRE-merge HEAD tree — reading HEAD after the ref moved
// would compare the merged tree against itself and never delete. Refs: MGIT-15
func TestMergeStore_CreateMergeCommit_NoFF_DeletesRemovedFileFromDisk(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()
	bs := NewBranchStore(repo)
	ms := NewMergeStore(repo)
	ws := NewWorktreeStore(repo)
	cs := NewCommitStore(repo)

	// Base shared by both branches has shared.go.
	baseHash := createCommitWithFile(t, repo, "shared.go", "package shared\n", "MGIT-5.1")

	// Feature deletes shared.go (stage the path with the file removed from disk).
	tid, _ := model.ParseTaskID("MGIT-5.2")
	require.NoError(t, bs.CreateBranch(ctx, &model.Branch{Name: "feature", HeadCommit: baseHash, TaskID: tid}))
	require.NoError(t, bs.SwitchBranch(ctx, "feature"))
	require.NoError(t, os.Remove(filepath.Join(repo.Root(), "shared.go")))
	require.NoError(t, ws.Add(ctx, "shared.go"))
	dc := makeTestModelCommit(t, "MGIT-5.3")
	dc.Message = "[MGIT:MGIT-5.3] delete shared"
	featureHash, err := cs.CreateCommit(ctx, dc)
	require.NoError(t, err)

	// main diverges. main's tree still contains shared.go; SwitchBranch only
	// moves the HEAD ref (it does not materialize), so write shared.go back to
	// reflect main's pre-merge working tree — the state the merge must reconcile.
	require.NoError(t, bs.SwitchBranch(ctx, "main"))
	createCommitWithFile(t, repo, "main_only.go", "package main\n", "MGIT-5.4")
	require.NoError(t, os.WriteFile(filepath.Join(repo.Root(), "shared.go"), []byte("package shared\n"), 0o600))
	require.FileExists(t, filepath.Join(repo.Root(), "shared.go"))

	mergeHash, err := ms.CreateMergeCommit(ctx, "merge feature into main", featureHash)
	require.NoError(t, err)

	files := mergeCommitFiles(t, repo, mergeHash)
	assert.NotContains(t, files, "shared.go", "source's deletion must be reflected in the merge tree")
	_, statErr := os.Stat(filepath.Join(repo.Root(), "shared.go"))
	assert.True(t, os.IsNotExist(statErr),
		"merge must remove the source-deleted file from disk (deletion pass keyed off pre-merge HEAD)")
}

// TestMergeStore_CreateMergeCommit_DirtyTree_Refuses verifies a merge refuses to
// clobber uncommitted changes to a tracked file (mirroring Checkout's guard):
// it returns ErrRollbackConflict and leaves the dirty edit intact. Refs: MGIT-15
func TestMergeStore_CreateMergeCommit_DirtyTree_Refuses(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()
	bs := NewBranchStore(repo)
	ms := NewMergeStore(repo)

	baseHash := createCommitWithFile(t, repo, "keep.go", "v1\n", "MGIT-6.1")

	tid, _ := model.ParseTaskID("MGIT-6.2")
	require.NoError(t, bs.CreateBranch(ctx, &model.Branch{Name: "feature", HeadCommit: baseHash, TaskID: tid}))
	require.NoError(t, bs.SwitchBranch(ctx, "feature"))
	featureHash := createCommitWithFile(t, repo, "feat_only.go", "package feat\n", "MGIT-6.3")

	require.NoError(t, bs.SwitchBranch(ctx, "main"))
	createCommitWithFile(t, repo, "main_only.go", "package main\n", "MGIT-6.4")

	// Uncommitted edit to a tracked file: the merge must not silently overwrite it.
	keep := filepath.Join(repo.Root(), "keep.go")
	require.NoError(t, os.WriteFile(keep, []byte("DIRTY UNSAVED WORK\n"), 0o600))

	_, err := ms.CreateMergeCommit(ctx, "merge feature into main", featureHash)
	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrRollbackConflict, "a dirty tracked tree must block the merge")

	got, rerr := os.ReadFile(keep) //nolint:gosec // test-controlled path under t.TempDir
	require.NoError(t, rerr)
	assert.Equal(t, "DIRTY UNSAVED WORK\n", string(got), "the uncommitted edit must be preserved")
}

// TestMergeStore_CreateMergeCommit_PreservesExecutableMode verifies the merged
// tree preserves a source file's executable bit rather than collapsing it to a
// regular blob. Refs: MGIT-15, MGIT-14.7 (#3)
func TestMergeStore_CreateMergeCommit_PreservesExecutableMode(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()
	bs := NewBranchStore(repo)
	ms := NewMergeStore(repo)
	ws := NewWorktreeStore(repo)
	cs := NewCommitStore(repo)

	baseHash := createCommitWithFile(t, repo, "base.go", "package base\n", "MGIT-3.1")

	tid, _ := model.ParseTaskID("MGIT-3.2")
	require.NoError(t, bs.CreateBranch(ctx, &model.Branch{Name: "feature", HeadCommit: baseHash, TaskID: tid}))
	require.NoError(t, bs.SwitchBranch(ctx, "feature"))

	script := filepath.Join(repo.Root(), "run.sh")
	writeExecutable(t, script, "#!/bin/sh\necho hi\n")
	require.NoError(t, ws.Add(ctx, "run.sh"))
	fc := makeTestModelCommit(t, "MGIT-3.3")
	fc.Message = "[MGIT:MGIT-3.3] add script"
	featureHash, err := cs.CreateCommit(ctx, fc)
	require.NoError(t, err)

	require.NoError(t, bs.SwitchBranch(ctx, "main"))
	createCommitWithFile(t, repo, "main_only.go", "package main\n", "MGIT-3.4")

	mergeHash, err := ms.CreateMergeCommit(ctx, "merge feature into main", featureHash)
	require.NoError(t, err)

	files := mergeCommitFiles(t, repo, mergeHash)
	require.Contains(t, files, "run.sh")
	assert.Equal(t, filemode.Executable, files["run.sh"].mode, "executable bit must survive the merge")
}

// TestMergeStore_FastForward_MaterializesWorkingTree verifies that a
// fast-forward updates the working tree on disk to the advanced tip (a file
// that exists only at the advanced commit appears on disk), rather than leaving
// it stale. Refs: MGIT-15
func TestMergeStore_FastForward_MaterializesWorkingTree(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()
	bs := NewBranchStore(repo)
	ms := NewMergeStore(repo)

	baseHash, err := repo.Head()
	require.NoError(t, err)

	tid, _ := model.ParseTaskID("MGIT-4.1")
	require.NoError(t, bs.CreateBranch(ctx, &model.Branch{Name: "target", HeadCommit: baseHash, TaskID: tid}))

	advancedHash := createCommitWithFile(t, repo, "ff.go", "package ff\n", "MGIT-4.2")

	// Switch HEAD to target (which is still at base) and clear disk of ff.go to
	// prove the fast-forward materializes the advanced tip.
	require.NoError(t, bs.SwitchBranch(ctx, "target"))
	require.NoError(t, os.Remove(filepath.Join(repo.Root(), "ff.go")))

	require.NoError(t, ms.FastForward(ctx, "target", advancedHash))

	got, err := os.ReadFile(filepath.Join(repo.Root(), "ff.go"))
	require.NoError(t, err, "fast-forward must materialize the advanced tip onto disk")
	assert.Equal(t, "package ff\n", string(got))
}
