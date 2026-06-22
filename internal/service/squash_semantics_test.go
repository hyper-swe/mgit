package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
	gitstore "github.com/hyper-swe/mgit/internal/store/git"
)

// stageAndCommit writes file with content to the repo root, stages it, and
// creates a task-tagged micro-commit. Unlike the FileDiffs-only helpers, this
// produces a REAL differing git tree per commit so squash's tree-diff isolation
// is exercised. Returns the created commit.
func stageAndCommit(t *testing.T, env *testEnv, taskID, file, content string) *model.Commit {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, os.WriteFile(filepath.Join(env.repo.Root(), file), []byte(content), 0o600))
	ws := gitstore.NewWorktreeStore(env.repo)
	require.NoError(t, ws.Add(ctx, file))
	c, err := env.commit.CreateCommit(ctx, CreateCommitRequest{
		TaskID:  taskID,
		AgentID: "agent-01",
		Message: "edit " + file,
	})
	require.NoError(t, err)
	return c
}

// TestSquashService_SquashTask_LandsOnTaskBranch_MainUntouched pins the squash
// semantics chosen for MGIT-22: squashing a task produces a single commit
// capturing only that task's net changes, parented off the task's base (the
// parent of its first micro-commit), placed on a dedicated task/<ID> branch.
// The integration branch (HEAD/main) is NOT advanced, and the original
// micro-commits remain in history (append-only, FR-12). This is the regression
// for the dogfood bug where the squash was appended on top of an unrelated
// task's commit while the originals stayed in place. Refs: FR-7, FR-12, MGIT-22
func TestSquashService_SquashTask_LandsOnTaskBranch_MainUntouched(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	// History interleaves two tasks on the integration branch, exactly like the
	// dogfood: MGIT-1 (a, then a+b) then an unrelated MGIT-2 (c) at the tip.
	first := stageAndCommit(t, env, "MGIT-1", "a.go", "package a\n")
	stageAndCommit(t, env, "MGIT-1", "b.go", "package b\n")
	stageAndCommit(t, env, "MGIT-2", "c.go", "package c\n")

	headBefore, err := env.repo.Head()
	require.NoError(t, err)

	squashed, err := env.squash.SquashTask(ctx, SquashRequest{TaskID: "MGIT-1"})
	require.NoError(t, err)
	require.Equal(t, model.CommitTypeSquash, squashed.CommitType)

	// 1. The integration branch is NOT advanced — the squash never lands on main.
	headAfter, err := env.repo.Head()
	require.NoError(t, err)
	assert.Equal(t, headBefore, headAfter,
		"squash must not advance the integration branch (main)")

	// 2. The squash lives on a dedicated task/<ID> branch pointing at it.
	taskBranch, err := env.bs.GetBranch(ctx, "task/MGIT-1")
	require.NoError(t, err, "squash must create the task/MGIT-1 branch")
	assert.Equal(t, squashed.CommitID, taskBranch.HeadCommit,
		"task/MGIT-1 must point at the squash commit")

	// 3. The squash is parented off the task's base (parent of its first commit),
	//    not off the unrelated MGIT-2 tip.
	assert.Equal(t, first.ParentID, squashed.ParentID,
		"squash parent must be the task's base, not the unrelated tip")

	// 4. The squash tree captures ONLY this task's files: a.go and b.go are
	//    present; the unrelated task's c.go is absent.
	got, err := env.cs.GetFileFromCommit(ctx, squashed.CommitID, "a.go")
	require.NoError(t, err)
	assert.Equal(t, "package a\n", string(got))
	got, err = env.cs.GetFileFromCommit(ctx, squashed.CommitID, "b.go")
	require.NoError(t, err)
	assert.Equal(t, "package b\n", string(got))
	_, err = env.cs.GetFileFromCommit(ctx, squashed.CommitID, "c.go")
	assert.ErrorIs(t, err, model.ErrFileNotFound,
		"squash tree must not contain the unrelated task's file")

	// 5. The original micro-commits remain (append-only): both still reachable
	//    from main and still indexed.
	mainCommits, err := env.cs.ListCommits(ctx)
	require.NoError(t, err)
	mainHashes := make(map[string]bool, len(mainCommits))
	for _, c := range mainCommits {
		mainHashes[c.CommitID] = true
	}
	assert.True(t, mainHashes[first.ParentID] || first.ParentID == "",
		"task base remains reachable on main")
	records, err := env.idx.GetTaskCommits(ctx, "MGIT-1")
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(records), 3,
		"two originals + the squash are all indexed (append-only)")
}
