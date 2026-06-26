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

// TestSquash_PinnedForkBase_UnchangedByResync is the critical data-integrity
// property of ADR-008 §4: a task's squash is computed against the base it
// forked from, and a base resync of UNRELATED local work (advancing the shared
// base) must not change that — the squash base and net diff stay identical.
// Refs: MGIT-35, ADR-008 §3,§4
func TestSquash_PinnedForkBase_UnchangedByResync(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	// Establish a base with a file, then fork a task branch at that pinned tip.
	writeProjectFile(t, env, "base.go", "base v1\n")
	svc := newWorktreeSvcWithSync(env, "git-A")
	wt, err := svc.Add(ctx, model.WorktreeAddOptions{
		Path: filepath.Join(t.TempDir(), "wt"), TaskID: "MGIT-2.1",
	})
	require.NoError(t, err)
	pinnedBase := wt.ForkBase

	// Commit task work on the bound branch via a linked store (production path),
	// and index the commit so SquashTask can find it.
	taskCommit := commitOnTaskBranch(t, env, wt.Branch, "MGIT-2.1", "feature.go", "feature\n")
	require.NoError(t, env.idx.AddCommitToTask(ctx, "MGIT-2.1", taskCommit, "ch", "dev", 0))

	before, err := env.squash.SquashTask(ctx, SquashRequest{TaskID: "MGIT-2.1", DryRun: true})
	require.NoError(t, err)

	// Unrelated local work drifts + resyncs the shared base forward.
	writeProjectFile(t, env, "unrelated.go", "noise\n")
	require.NoError(t, newSyncService(env, "git-B", "").EnsureSynced(ctx))
	movedBase, err := env.repo.Head()
	require.NoError(t, err)
	require.NotEqual(t, pinnedBase, movedBase, "precondition: shared base advanced")

	after, err := env.squash.SquashTask(ctx, SquashRequest{TaskID: "MGIT-2.1", DryRun: true})
	require.NoError(t, err)

	// The squash's net diff is identical before and after the resync — the
	// task's fork-base did not shift under it.
	assert.Equal(t, before.FileDiffs, after.FileDiffs,
		"resync must not corrupt a pinned task's squash diff")
}

// TestSquash_PinnedForkBaseDiverged_FailsLoud verifies the ADR-008 §4 pin is
// LOAD-BEARING: if a task's recorded fork-base ever diverges from the base
// squash computes (the first micro-commit's parent) — i.e. the task branch was
// retargeted/rewritten — squash fails loud rather than exporting a corrupt net
// diff. Refs: MGIT-35, ADR-008 §4
func TestSquash_PinnedForkBaseDiverged_FailsLoud(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	writeProjectFile(t, env, "base.go", "base v1\n")
	svc := newWorktreeSvcWithSync(env, "git-A")
	wt, err := svc.Add(ctx, model.WorktreeAddOptions{
		Path: filepath.Join(t.TempDir(), "wt"), TaskID: "MGIT-2.1",
	})
	require.NoError(t, err)

	taskCommit := commitOnTaskBranch(t, env, wt.Branch, "MGIT-2.1", "feature.go", "feature\n")
	require.NoError(t, env.idx.AddCommitToTask(ctx, "MGIT-2.1", taskCommit, "ch", "dev", 0))

	// Corrupt the registry's pinned fork-base so it no longer matches the task's
	// actual fork point (simulating a retarget/rewrite).
	require.NoError(t, env.idx.DeleteWorktree(ctx, wt.Path))
	wt.ForkBase = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	require.NoError(t, env.idx.InsertWorktree(ctx, wt))

	_, err = env.squash.SquashTask(ctx, SquashRequest{TaskID: "MGIT-2.1", DryRun: true})
	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrVerificationFailed)
}

// commitOnTaskBranch commits a file onto branch via a linked store bound to it
// (mirroring how a worktree commits), returning the commit hash. It shares the
// parent's object store, so the parent can read the commit back.
func commitOnTaskBranch(t *testing.T, env *testEnv, branch, taskID, file, content string) string {
	t.Helper()
	ctx := context.Background()
	wtRoot := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(wtRoot, ".mgit"), 0o750))
	linked, err := gitstore.OpenLinked(wtRoot, env.repo.MgitDir(), branch, fixedClock())
	require.NoError(t, err)
	t.Cleanup(func() { _ = linked.Close() })

	require.NoError(t, os.WriteFile(filepath.Join(wtRoot, file), []byte(content), 0o600))
	require.NoError(t, gitstore.NewWorktreeStore(linked).Add(ctx, file))
	c := &model.Commit{AgentID: "dev", Message: "[MGIT:" + taskID + "] add " + file}
	hash, err := gitstore.NewCommitStore(linked).CreateCommit(ctx, c)
	require.NoError(t, err)
	return hash
}
