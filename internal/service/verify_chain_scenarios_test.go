package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// TestVerifyService_VerifyCommitChain_BrokenParent_Error: a sequence whose
// adjacency does not match parent links is reported as ErrChainBroken.
func TestVerifyService_VerifyCommitChain_BrokenParent_Error(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()
	vs := NewVerifyService(env.cs, env.idx)

	a := stageAndCommit(t, env, "MGIT-1", "a.go", "package a\n")
	b := stageAndCommit(t, env, "MGIT-1", "b.go", "package b\n")

	// In real lineage b's parent is a. Passing them reversed breaks the chain:
	// at index 1 (a) the parent is the genesis commit, not b.
	err := vs.VerifyCommitChain(ctx, []string{b.CommitID, a.CommitID})
	assert.ErrorIs(t, err, model.ErrChainBroken, "reversed lineage must be a broken chain")

	// The correctly-ordered chain validates.
	require.NoError(t, vs.VerifyCommitChain(ctx, []string{a.CommitID, b.CommitID}))
}

// TestVerifyService_VerifyCommitChain_MissingCommit_Error: a hash absent from
// the store fails the chain check (not a silent skip).
func TestVerifyService_VerifyCommitChain_MissingCommit_Error(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()
	vs := NewVerifyService(env.cs, env.idx)

	missing := "0123456789abcdef0123456789abcdef01234567"
	err := vs.VerifyCommitChain(ctx, []string{missing})
	assert.Error(t, err, "a missing commit must fail the chain verification")
}

// TestVerifyService_VerifyTaskCommits_BadRecord_Error: an index record pointing
// at a non-existent commit is detected when verifying the task.
func TestVerifyService_VerifyTaskCommits_BadRecord_Error(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()
	vs := NewVerifyService(env.cs, env.idx)

	// Index a record whose commit hash does not exist in the object store.
	bogus := "0123456789abcdef0123456789abcdef01234567"
	require.NoError(t, env.idx.AddCommitToTask(ctx, "MGIT-7.1", bogus, "deadbeef", "agent", 0))

	err := vs.VerifyTaskCommits(ctx, "MGIT-7.1")
	assert.Error(t, err, "a task record pointing at a missing commit must be flagged")
}
