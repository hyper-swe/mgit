package service

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
	gitstore "github.com/hyper-swe/mgit/internal/store/git"
)

// TestService_IndexFailure_Propagates verifies that when the SQLite index is
// unavailable (its connection is closed), every service operation that depends
// on it fails LOUDLY rather than returning a wrong/empty result. This is a real
// safety scenario (a corrupt/locked index must not be silently swallowed), and
// it exercises the index-error branches across the service layer. The pattern
// (close env.idx to force a real DB error) mirrors the existing
// TestCommitService_GetCommit_IndexFailurePropagates. Refs: FR-12
func TestService_IndexFailure_Propagates(t *testing.T) {
	cases := []struct {
		name string
		// run sets up any needed data, closes the index, then invokes the
		// operation under test and returns its error.
		run func(t *testing.T, env *testEnv, ctx context.Context) error
	}{
		{"commit_create", func(t *testing.T, env *testEnv, ctx context.Context) error {
			require.NoError(t, env.idx.Close())
			_, err := env.commit.CreateCommit(ctx, CreateCommitRequest{TaskID: "MGIT-1", AgentID: "a", Message: "m"})
			return err
		}},
		{"commit_list", func(t *testing.T, env *testEnv, ctx context.Context) error {
			_, err := env.commit.CreateCommit(ctx, CreateCommitRequest{TaskID: "MGIT-1", AgentID: "a", Message: "m"})
			require.NoError(t, err)
			require.NoError(t, env.idx.Close())
			_, err = env.commit.ListCommits(ctx) // enrichProvenance hits the closed index
			return err
		}},
		{"squash", func(t *testing.T, env *testEnv, ctx context.Context) error {
			require.NoError(t, env.idx.Close())
			_, err := env.squash.SquashTask(ctx, SquashRequest{TaskID: "MGIT-1"})
			return err
		}},
		{"rollback", func(t *testing.T, env *testEnv, ctx context.Context) error {
			require.NoError(t, env.idx.Close())
			_, err := env.rollbk.RollbackTask(ctx, RollbackRequest{TaskID: "MGIT-1"})
			return err
		}},
		{"diff_task", func(t *testing.T, env *testEnv, ctx context.Context) error {
			ds := NewDiffService(gitstore.NewDiffStore(env.repo), env.cs, env.idx)
			require.NoError(t, env.idx.Close())
			_, err := ds.DiffTask(ctx, "MGIT-1")
			return err
		}},
		{"verify_task", func(t *testing.T, env *testEnv, ctx context.Context) error {
			vs := NewVerifyService(env.cs, env.idx)
			require.NoError(t, env.idx.Close())
			return vs.VerifyTaskCommits(ctx, "MGIT-1")
		}},
		// NOTE: VerifyIndexIntegrity is intentionally excluded — it treats a
		// GetCommitTask error as "commit not indexed" (appends an issue) rather
		// than propagating the DB failure, so a closed index yields issues, not
		// an error. That swallow-DB-error behavior mirrors the bug MGIT-19 fixed
		// in enrichProvenance and is flagged as a follow-up, not asserted here.
		{"bundle_export", func(t *testing.T, env *testEnv, ctx context.Context) error {
			bs := NewBundleService(env.idx, fixedClock())
			require.NoError(t, env.idx.Close())
			_, err := bs.Export(ctx, []string{"MGIT-1"})
			return err
		}},
		{"branch_create", func(t *testing.T, env *testEnv, ctx context.Context) error {
			require.NoError(t, env.idx.Close())
			_, err := env.branch.CreateBranch(ctx, "MGIT-1")
			return err
		}},
		{"worktree_add", func(t *testing.T, env *testEnv, ctx context.Context) error {
			wtSvc := NewWorktreeService(env.idx, env.branch, env.wt, fixedClock())
			require.NoError(t, env.idx.Close())
			_, err := wtSvc.Add(ctx, model.WorktreeAddOptions{
				Path: filepath.Join(t.TempDir(), "wt"), TaskID: "MGIT-1", AgentID: "a",
			})
			return err
		}},
		{"worktree_list", func(t *testing.T, env *testEnv, ctx context.Context) error {
			wtSvc := NewWorktreeService(env.idx, env.branch, env.wt, fixedClock())
			require.NoError(t, env.idx.Close())
			_, err := wtSvc.List(ctx)
			return err
		}},
		{"get_task_commits", func(t *testing.T, env *testEnv, ctx context.Context) error {
			require.NoError(t, env.idx.Close())
			_, err := env.commit.GetTaskCommits(ctx, "MGIT-1")
			return err
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := setupTestEnv(t)
			ctx := context.Background()
			err := tc.run(t, env, ctx)
			require.Error(t, err, "%s must propagate the index failure, not swallow it", tc.name)
		})
	}
}
