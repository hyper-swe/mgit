package git

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/stretchr/testify/require"
)

func TestFastForwardDelivered_RejectsInvalidDestinationOrHistory(t *testing.T) {
	ctx := context.Background()
	repo := initTestRepo(t)
	base := createCommitWithFile(t, repo, "tracked.txt", "before\n", "MGIT-9.2")
	tip := createCommitWithFile(t, repo, "tracked.txt", "after\n", "MGIT-9.2")
	branch, err := repo.CurrentBranch()
	require.NoError(t, err)
	const other = "other-view"
	branches := NewBranchStore(repo)
	require.NoError(t, branches.CreateBranch(ctx, &model.Branch{Name: other, HeadCommit: base}))
	merge := NewMergeStore(repo)

	for _, name := range []string{"missing branch", "missing target", "other view", "older target"} {
		t.Run(name, func(t *testing.T) {
			switch name {
			case "missing branch":
				require.ErrorIs(t, merge.FastForwardDelivered(ctx, "missing", tip), model.ErrBranchNotFound)
				_, err := branches.GetBranch(ctx, "missing")
				require.ErrorIs(t, err, model.ErrBranchNotFound)
			case "missing target":
				require.ErrorIs(t, merge.FastForwardDelivered(ctx, branch, strings.Repeat("f", 40)), model.ErrCommitNotFound)
			case "other view":
				err := merge.FastForwardDelivered(ctx, other, tip)
				require.ErrorContains(t, err, "not bound to branch "+other)
			case "older target":
				err := merge.FastForwardDelivered(ctx, branch, base)
				var divergence *ProjectionDivergenceError
				require.ErrorAs(t, err, &divergence)
				require.Equal(t, tip, divergence.HostTip)
				require.Equal(t, base, divergence.GuestBase)
				require.Contains(t, err.Error(), "non-fast-forward")
			}
			current, err := repo.Head()
			require.NoError(t, err)
			require.Equal(t, tip, current)
			unbound, err := branches.GetBranch(ctx, other)
			require.NoError(t, err)
			require.Equal(t, base, unbound.HeadCommit)
			content, err := os.ReadFile(filepath.Join(repo.Root(), "tracked.txt"))
			require.NoError(t, err)
			require.Equal(t, "after\n", string(content))
		})
	}
}
