package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A source whose tip is already an ancestor of HEAD has nothing to merge, so
// every strategy reports "already up to date" and leaves HEAD where it was.
// Refs: MGIT-326
func TestMergeService_Merge_SourceAlreadyAncestorOfHead_IsUpToDate(t *testing.T) {
	for _, strategy := range []MergeStrategy{MergeAuto, MergeNoFF, MergeSquash} {
		t.Run(string(strategy), func(t *testing.T) {
			env, mergeSvc := setupMergeEnv(t)
			ctx := context.Background()

			_, err := env.branch.CreateBranch(ctx, "MGIT-10.3")
			require.NoError(t, err)
			require.NoError(t, env.branch.SwitchBranch(ctx, "task/MGIT-10.3"))
			_, err = env.commit.CreateCommit(ctx, CreateCommitRequest{
				AllowEmpty: true,
				TaskID:     "MGIT-10.3", AgentID: "a", Message: "branch commit",
			})
			require.NoError(t, err)
			require.NoError(t, env.branch.SwitchBranch(ctx, "main"))

			// The first merge fast-forwards main onto the task tip.
			_, err = mergeSvc.Merge(ctx, MergeRequest{SourceBranch: "task/MGIT-10.3"})
			require.NoError(t, err)
			headBefore, err := env.repo.Head()
			require.NoError(t, err)

			result, err := mergeSvc.Merge(ctx, MergeRequest{
				SourceBranch: "task/MGIT-10.3",
				Strategy:     strategy,
			})
			require.NoError(t, err)
			assert.Equal(t, "already up to date", result.Status)
			assert.False(t, result.FastFwd)
			assert.Equal(t, headBefore, result.MergedHash)

			headAfter, err := env.repo.Head()
			require.NoError(t, err)
			assert.Equal(t, headBefore, headAfter, "HEAD must not move")
		})
	}
}
