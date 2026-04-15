package git

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

func TestBranchStore_CreateBranch(t *testing.T) {
	repo := initTestRepo(t)
	bs := NewBranchStore(repo)
	ctx := context.Background()

	head, err := repo.Head()
	require.NoError(t, err)

	taskID, err := model.ParseTaskID("MGIT-1.2")
	require.NoError(t, err)

	branch := &model.Branch{
		Name:       "task/MGIT-1.2",
		HeadCommit: head,
		TaskID:     taskID,
		CreatedAt:  repo.Now(),
	}

	err = bs.CreateBranch(ctx, branch)
	assert.NoError(t, err, "CreateBranch must succeed")
}

func TestBranchStore_CreateBranch_AlreadyExists(t *testing.T) {
	repo := initTestRepo(t)
	bs := NewBranchStore(repo)
	ctx := context.Background()

	head, err := repo.Head()
	require.NoError(t, err)

	taskID, err := model.ParseTaskID("MGIT-1.2")
	require.NoError(t, err)

	branch := &model.Branch{
		Name:       "task/MGIT-1.2",
		HeadCommit: head,
		TaskID:     taskID,
	}

	err = bs.CreateBranch(ctx, branch)
	require.NoError(t, err)

	err = bs.CreateBranch(ctx, branch)
	assert.ErrorIs(t, err, model.ErrBranchAlreadyExists)
}

func TestBranchStore_GetBranch(t *testing.T) {
	repo := initTestRepo(t)
	bs := NewBranchStore(repo)
	ctx := context.Background()

	// main branch should exist from init
	branch, err := bs.GetBranch(ctx, "main")
	require.NoError(t, err, "GetBranch for main must succeed")
	assert.Equal(t, "main", branch.Name)
	assert.NotEmpty(t, branch.HeadCommit)
}

func TestBranchStore_GetBranch_NotFound(t *testing.T) {
	repo := initTestRepo(t)
	bs := NewBranchStore(repo)
	ctx := context.Background()

	_, err := bs.GetBranch(ctx, "nonexistent")
	assert.ErrorIs(t, err, model.ErrBranchNotFound)
}

func TestBranchStore_ListBranches(t *testing.T) {
	repo := initTestRepo(t)
	bs := NewBranchStore(repo)
	ctx := context.Background()

	branches, err := bs.ListBranches(ctx)
	require.NoError(t, err)
	// Should have at least "main"
	assert.GreaterOrEqual(t, len(branches), 1)

	names := make([]string, 0, len(branches))
	for _, b := range branches {
		names = append(names, b.Name)
	}
	assert.Contains(t, names, "main")
}

func TestBranchStore_SwitchBranch(t *testing.T) {
	repo := initTestRepo(t)
	bs := NewBranchStore(repo)
	ctx := context.Background()

	// Create a new branch
	head, err := repo.Head()
	require.NoError(t, err)

	taskID, err := model.ParseTaskID("MGIT-2.1")
	require.NoError(t, err)

	err = bs.CreateBranch(ctx, &model.Branch{
		Name:       "task/MGIT-2.1",
		HeadCommit: head,
		TaskID:     taskID,
	})
	require.NoError(t, err)

	// Switch to new branch
	err = bs.SwitchBranch(ctx, "task/MGIT-2.1")
	assert.NoError(t, err, "SwitchBranch must succeed")
}

func TestBranchStore_SwitchBranch_NotFound(t *testing.T) {
	repo := initTestRepo(t)
	bs := NewBranchStore(repo)
	ctx := context.Background()

	err := bs.SwitchBranch(ctx, "nonexistent")
	assert.ErrorIs(t, err, model.ErrBranchNotFound)
}

func TestBranchStore_DeleteBranch_RejectsUnmerged(t *testing.T) {
	repo := initTestRepo(t)
	bs := NewBranchStore(repo)
	ctx := context.Background()

	head, err := repo.Head()
	require.NoError(t, err)

	taskID, err := model.ParseTaskID("MGIT-3.1")
	require.NoError(t, err)

	err = bs.CreateBranch(ctx, &model.Branch{
		Name:       "task/MGIT-3.1",
		HeadCommit: head,
		TaskID:     taskID,
	})
	require.NoError(t, err)

	// Delete should reject because it's not merged
	err = bs.DeleteBranch(ctx, "task/MGIT-3.1", false)
	assert.Error(t, err, "DeleteBranch should reject unmerged branch")
}

func TestBranchStore_DeleteBranch_Force(t *testing.T) {
	repo := initTestRepo(t)
	bs := NewBranchStore(repo)
	ctx := context.Background()

	head, err := repo.Head()
	require.NoError(t, err)

	taskID, err := model.ParseTaskID("MGIT-3.2")
	require.NoError(t, err)

	err = bs.CreateBranch(ctx, &model.Branch{
		Name:       "task/MGIT-3.2",
		HeadCommit: head,
		TaskID:     taskID,
	})
	require.NoError(t, err)

	// Force delete should succeed
	err = bs.DeleteBranch(ctx, "task/MGIT-3.2", true)
	assert.NoError(t, err, "Force delete must succeed")

	// Verify branch is gone
	_, err = bs.GetBranch(ctx, "task/MGIT-3.2")
	assert.ErrorIs(t, err, model.ErrBranchNotFound)
}
