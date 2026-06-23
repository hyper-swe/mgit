package git

import (
	"context"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Reference / iterator / specific-hash fault coverage for the remaining store
// read-error branches. Refs: FR-12, NFR-5

func TestListBranches_IterFault_Errors(t *testing.T) {
	repo, fs, _ := faultRepoWithCommit(t)
	fs.failIterRefs = true
	_, err := NewBranchStore(repo).ListBranches(context.Background())
	assert.Error(t, err, "ListBranches must surface a ref-iteration failure")
}

func TestResolveCommitHash_IterFault_Errors(t *testing.T) {
	repo, fs, h := faultRepoWithCommit(t)
	fs.failIterObjects = true
	// An abbreviated prefix forces the object scan, which now faults.
	_, err := NewCommitStore(repo).GetCommit(context.Background(), h[:10])
	assert.Error(t, err, "resolving an abbreviated hash must surface an iteration failure")
}

func TestMerge_SourceCommitReadFault_Errors(t *testing.T) {
	repo, fs, _ := faultRepoWithCommit(t)
	cs := NewCommitStore(repo)
	bs := NewBranchStore(repo)
	ms := NewMergeStore(repo)
	ctx := context.Background()

	// Create a divergent source branch so the merge must read the source side.
	require.NoError(t, bs.SwitchBranch(ctx, "task/MGIT-1"))
	stageOneFile(t, repo, "b.go", "package b\n")
	scb := makeTestModelCommit(t, "MGIT-2")
	scb.FileDiffs = nil
	src, err := cs.CreateCommit(ctx, scb)
	require.NoError(t, err)
	require.NoError(t, bs.SwitchBranch(ctx, "main"))
	stageOneFile(t, repo, "c.go", "package c\n")
	mcb := makeTestModelCommit(t, "MGIT-1")
	mcb.FileDiffs = nil
	_, err = cs.CreateCommit(ctx, mcb)
	require.NoError(t, err)

	// Fault exactly the source commit's object so the merged-tree's source-side
	// read fails (the deeper netDelta/commitFiles wrap).
	fs.failHash = plumbing.NewHash(src)
	_, err = ms.CreateMergeCommit(ctx, "merge", src)
	assert.Error(t, err, "merge must fail when the source commit can't be read")
}

func TestSquash_SecondCommitReadFault_Errors(t *testing.T) {
	repo, fs, h1 := faultRepoWithCommit(t)
	cs := NewCommitStore(repo)
	ctx := context.Background()

	stageOneFile(t, repo, "b.go", "package b\n")
	c2 := makeTestModelCommit(t, "MGIT-1")
	c2.FileDiffs = nil
	h2, err := cs.CreateCommit(ctx, c2)
	require.NoError(t, err)

	// Fault the second task commit; the squash's net-change walk must surface it.
	fs.failHash = plumbing.NewHash(h2)
	sc := makeTestModelCommit(t, "MGIT-1")
	sc.FileDiffs = nil
	_, err = cs.CreateSquashCommit(ctx, SquashCommitParams{
		Commit: sc, TaskCommits: []string{h1, h2}, Branch: "task/sq",
	})
	assert.Error(t, err, "squash must fail when a task commit object can't be read")
}
