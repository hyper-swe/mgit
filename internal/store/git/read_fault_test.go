package git

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests fault the object store's READ path after a healthy setup, so the
// "couldn't read commit/tree/object" error branches across the store (which are
// unreachable with a healthy store) are exercised — mgit must surface a store
// read failure, never return a wrong/empty result. Refs: FR-12, NFR-5

// faultRepoWithCommit returns a fault repo that already has one task commit and
// a task branch, with faults still OFF, ready to toggle for the op under test.
func faultRepoWithCommit(t *testing.T) (*Repository, *faultStorer, string) {
	t.Helper()
	repo, fs := newFaultRepo(t)
	cs := NewCommitStore(repo)
	stageOneFile(t, repo, "a.go", "package a\n")
	c := makeTestModelCommit(t, "MGIT-1")
	c.FileDiffs = nil
	h, err := cs.CreateCommit(context.Background(), c)
	require.NoError(t, err)
	require.NoError(t, NewBranchStore(repo).CreateBranch(context.Background(), branchModel("task/MGIT-1", h)))
	return repo, fs, h
}

func TestStatus_ObjectReadFault_Errors(t *testing.T) {
	repo, fs, _ := faultRepoWithCommit(t)
	fs.failGetObject = true
	_, err := NewWorktreeStore(repo).Status(context.Background())
	assert.Error(t, err, "Status must surface a HEAD-tree read fault")
}

func TestIsClean_ObjectReadFault_Errors(t *testing.T) {
	repo, fs, _ := faultRepoWithCommit(t)
	fs.failGetObject = true
	_, _, err := NewWorktreeStore(repo).IsClean(context.Background())
	assert.Error(t, err)
}

func TestHeadFiles_ObjectReadFault_Errors(t *testing.T) {
	repo, fs, _ := faultRepoWithCommit(t)
	fs.failGetObject = true
	_, err := repo.headFiles()
	assert.Error(t, err)
}

func TestListCommits_ObjectReadFault_Errors(t *testing.T) {
	repo, fs, _ := faultRepoWithCommit(t)
	fs.failGetObject = true
	_, err := NewCommitStore(repo).ListCommits(context.Background())
	assert.Error(t, err)
}

func TestCheckout_ObjectReadFault_Errors(t *testing.T) {
	repo, fs, _ := faultRepoWithCommit(t)
	fs.failGetObject = true
	err := NewWorktreeStore(repo).Checkout(context.Background(), "task/MGIT-1")
	assert.Error(t, err, "checkout must fail when the tree can't be read")
}

func TestMaterializeBranchTo_ObjectReadFault_Errors(t *testing.T) {
	repo, fs, _ := faultRepoWithCommit(t)
	fs.failGetObject = true
	err := NewWorktreeStore(repo).MaterializeBranchTo(context.Background(), "task/MGIT-1", t.TempDir())
	assert.Error(t, err, "materialize must fail when the commit can't be read")
}

func TestCreateCommit_TreeReadFault_Errors(t *testing.T) {
	repo, fs, _ := faultRepoWithCommit(t)
	stageOneFile(t, repo, "b.go", "package b\n")
	fs.failGetObject = true
	c := makeTestModelCommit(t, "MGIT-1")
	c.FileDiffs = nil
	_, err := NewCommitStore(repo).CreateCommit(context.Background(), c)
	assert.Error(t, err, "commit must fail when the HEAD tree can't be read")
}

func TestSquash_ObjectReadFault_Errors(t *testing.T) {
	repo, fs, h := faultRepoWithCommit(t)
	fs.failGetObject = true
	c := makeTestModelCommit(t, "MGIT-1")
	c.FileDiffs = nil
	_, err := NewCommitStore(repo).CreateSquashCommit(context.Background(), SquashCommitParams{
		Commit: c, TaskCommits: []string{h}, Branch: "task/sq",
	})
	assert.Error(t, err, "squash must fail when a task commit can't be read")
}

func TestMerge_ObjectReadFault_Errors(t *testing.T) {
	repo, fs, h := faultRepoWithCommit(t)
	fs.failGetObject = true
	_, err := NewMergeStore(repo).CreateMergeCommit(context.Background(), "merge", h)
	assert.Error(t, err, "merge must fail when commits can't be read")
}

func TestDiffCommits_ObjectReadFault_Errors(t *testing.T) {
	repo, fs, h := faultRepoWithCommit(t)
	fs.failGetObject = true
	_, err := NewDiffStore(repo).DiffCommits(context.Background(), h, h)
	assert.Error(t, err)
}
