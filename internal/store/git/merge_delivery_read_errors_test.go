package git

import (
	"context"
	"io/fs"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDeliveryReportsUnreadableHistory(t *testing.T) {
	repo, faults, tip := faultRepoWithCommit(t)
	branch, err := repo.CurrentBranch()
	require.NoError(t, err)
	ctx := context.Background()
	merge := NewMergeStore(repo)
	faults.failGetObject = true

	// Even delivery of the current tip still needs a readable tree.
	require.ErrorContains(t, merge.FastForwardDelivered(ctx, branch, tip), "get HEAD commit")
	require.ErrorContains(t, merge.VerifyDelivered(ctx, tip), "get HEAD commit")
	head, entry, err := repo.ProjectionRecoveryCheckpoint("a.go")
	require.ErrorContains(t, err, "get HEAD commit")
	require.Empty(t, head)
	require.Nil(t, entry)

	faults.failGetObject = false
	current, err := repo.Head()
	require.NoError(t, err)
	require.Equal(t, tip, current)
	data, err := fs.ReadFile(os.DirFS(repo.Root()), "a.go")
	require.NoError(t, err)
	require.Equal(t, "package a\n", string(data))
	require.NoError(t, merge.VerifyDelivered(ctx, tip))
}

func TestDeliveryReadbackNeedsResolvableHead(t *testing.T) {
	repo, faults, tip := faultRepoWithCommit(t)
	faults.failGetReference = true
	require.Error(t, NewMergeStore(repo).VerifyDelivered(context.Background(), tip))
	head, entry, err := repo.ProjectionRecoveryCheckpoint("a.go")
	require.Error(t, err)
	require.Empty(t, head)
	require.Nil(t, entry)

	faults.failGetReference = false
	head, entry, err = repo.ProjectionRecoveryCheckpoint("a.go")
	require.NoError(t, err)
	require.Equal(t, tip, head)
	require.NotNil(t, entry)
	require.Equal(t, "a.go", entry.Path)
	require.True(t, entry.TargetExists)
}
