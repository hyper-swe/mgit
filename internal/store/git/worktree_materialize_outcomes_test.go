package git

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/stretchr/testify/require"
)

func TestMaterializeBranchTo_ErrorOutcomes(t *testing.T) {
	for _, scenario := range []string{"unknown branch", "destination conflict", "unreadable include list"} {
		t.Run(scenario, func(t *testing.T) {
			repo := initTestRepo(t)
			tip := createCommitWithFile(t, repo, "tracked.txt", "source", "MGIT-9.2")
			branch, err := repo.CurrentBranch()
			require.NoError(t, err)
			dest := filepath.Join(t.TempDir(), "worktree")
			entry := filepath.Join(dest, "tracked.txt")
			materialize := NewWorktreeStore(repo).MaterializeBranchTo
			switch scenario {
			case "unknown branch":
				err := materialize(context.Background(), "missing", dest)
				require.ErrorIs(t, err, model.ErrBranchNotFound)
				_, err = os.Stat(dest)
				require.ErrorIs(t, err, os.ErrNotExist)
			case "destination conflict":
				require.NoError(t, os.MkdirAll(entry, 0o700))
				keep := filepath.Join(entry, "keep")
				require.NoError(t, os.WriteFile(keep, []byte("existing"), 0o600))
				err := materialize(context.Background(), branch, dest)
				require.ErrorContains(t, err, branch)
				require.ErrorContains(t, err, "tracked.txt")
				data, err := os.ReadFile(keep)
				require.NoError(t, err)
				require.Equal(t, "existing", string(data))
			case "unreadable include list":
				require.NoError(t, os.Mkdir(filepath.Join(repo.MgitDir(), seedIncludeFileName), 0o700))
				err := materialize(context.Background(), branch, dest)
				require.ErrorContains(t, err, branch)
				require.ErrorContains(t, err, "seed-include")
				data, err := os.ReadFile(entry)
				require.NoError(t, err)
				require.Equal(t, "source", string(data))
			}
			head, err := repo.Head()
			require.NoError(t, err)
			require.Equal(t, tip, head)
		})
	}
}
