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
	"github.com/hyper-swe/mgit/internal/store/gitref"
)

// newWorktreeSvcWithSync wires a WorktreeService with the auto-housekeeping
// (sync + pinned fork-base) dependencies, using a fake git-state reader.
func newWorktreeSvcWithSync(env *testEnv, gitHead string) *WorktreeService {
	sync := newSyncService(env, gitHead, "")
	return NewWorktreeService(env.idx, env.branch, env.wt, fixedClock()).
		WithSync(sync, env.repo, env.cs)
}

// newWorktreeSvcOverEmptyGit is a WorktreeService whose project git has one
// commit with an EMPTY tree, so every file in the checkout is uncommitted.
func newWorktreeSvcOverEmptyGit(env *testEnv) *WorktreeService {
	sync := newSyncService(env, "head-A", "").
		withCommittedReader(func(string) (map[string]string, error) { return map[string]string{}, nil }).
		withCommittedFilesReader(func(string) ([]gitref.CommittedFile, string, error) { return nil, "head-A", nil })
	return NewWorktreeService(env.idx, env.branch, env.wt, fixedClock()).WithSync(sync, env.repo, env.cs)
}

// TestWorktreeAdd_IncludeUncommitted_CarriesUnpushedFoundation verifies the
// opt-in of ADR-008 §2 as amended (MGIT-283): with IncludeUncommitted the
// worktree is materialized FROM the resynced local base, so a file present
// only in the local working tree lands in the worktree, and is reported as
// captured. Refs: MGIT-35, MGIT-283, ADR-008 §2
func TestWorktreeAdd_IncludeUncommitted_CarriesUnpushedFoundation(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	writeProjectFile(t, env, "foundation.go", "package foundation\n")
	dest := filepath.Join(t.TempDir(), "wt")

	wt, err := newWorktreeSvcOverEmptyGit(env).Add(ctx,
		model.WorktreeAddOptions{Path: dest, TaskID: "MGIT-1.1", IncludeUncommitted: true})
	require.NoError(t, err)

	got, err := os.ReadFile(filepath.Join(dest, "foundation.go")) //nolint:gosec // test path
	require.NoError(t, err)
	assert.Equal(t, "package foundation\n", string(got))
	assert.Contains(t, wt.UncommittedPaths, "foundation.go")
	assert.True(t, wt.UncommittedIncluded)
}

// TestWorktreeAdd_Default_LeavesUncommittedOutAndReportsIt is the default of
// ADR-008 §2 as amended (MGIT-283): a NEW task's base is git's committed tree,
// so a file only in the checkout's working tree is left out of the task and
// reported as left out. Refs: MGIT-283, ADR-008 §2
func TestWorktreeAdd_Default_LeavesUncommittedOutAndReportsIt(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	writeProjectFile(t, env, "private.go", "package private\n")
	dest := filepath.Join(t.TempDir(), "wt")

	wt, err := newWorktreeSvcOverEmptyGit(env).Add(ctx, model.WorktreeAddOptions{Path: dest, TaskID: "MGIT-1.1"})
	require.NoError(t, err)

	_, statErr := os.Stat(filepath.Join(dest, "private.go"))
	assert.True(t, os.IsNotExist(statErr), "an uncommitted file is not in the task")
	assert.Contains(t, wt.UncommittedPaths, "private.go")
	assert.False(t, wt.UncommittedIncluded)
}

// TestWorktreeAdd_PinnedBase_UnchangedByLaterResync is the critical ADR-008 §4
// property: once a task worktree is created, advancing the shared base (a later
// resync of unrelated local work) must NOT shift the task's pinned fork-base.
// Refs: MGIT-35, ADR-008 §3,§4
func TestWorktreeAdd_PinnedBase_UnchangedByLaterResync(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	writeProjectFile(t, env, "a.go", "v1\n")
	svc := newWorktreeSvcWithSync(env, "head-1")
	wt, err := svc.Add(ctx, model.WorktreeAddOptions{Path: filepath.Join(t.TempDir(), "wt"), TaskID: "MGIT-1.1"})
	require.NoError(t, err)
	pinned := wt.ForkBase
	require.NotEmpty(t, pinned)

	// Unrelated local work drifts the shared base and triggers a resync.
	writeProjectFile(t, env, "unrelated.go", "noise\n")
	require.NoError(t, newSyncService(env, "head-2", "").EnsureSyncedForNewWorktree(ctx))
	newBase, err := env.repo.Head()
	require.NoError(t, err)
	require.NotEqual(t, pinned, newBase, "precondition: the shared base advanced")

	// The task's pinned fork-base in the registry is unchanged.
	reg, err := env.idx.GetWorktree(ctx, wt.Path)
	require.NoError(t, err)
	assert.Equal(t, pinned, reg.ForkBase, "resync must not shift a pinned fork-base")

	// The task branch tip still points at the pinned fork-base (not the new base).
	br, err := env.branch.GetBranch(ctx, wt.Branch)
	require.NoError(t, err)
	assert.Equal(t, pinned, br.HeadCommit)
}

// TestWorktreeAdd_ExplicitBase_PinsToRef verifies `mgit work --base <ref>`: the
// task forks off the explicit ref, not the auto-resynced local base.
// Refs: MGIT-35, ADR-008 §4
func TestWorktreeAdd_ExplicitBase_PinsToRef(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	// An earlier base commit to pin to.
	writeProjectFile(t, env, "a.go", "v1\n")
	require.NoError(t, env.wt.Add(ctx, "a.go"))
	c, err := env.commit.CreateCommit(ctx, CreateCommitRequest{
		TaskID: "MGIT-9.9", AgentID: "dev", Message: "earlier base",
	})
	require.NoError(t, err)
	earlier := c.CommitID

	// Drift forward so the local base differs from `earlier`.
	writeProjectFile(t, env, "b.go", "v2\n")

	svc := newWorktreeSvcWithSync(env, "head-X")
	wt, err := svc.Add(ctx, model.WorktreeAddOptions{
		Path: filepath.Join(t.TempDir(), "wt"), TaskID: "MGIT-1.1", Base: earlier,
	})
	require.NoError(t, err)
	assert.Equal(t, earlier, wt.ForkBase, "--base must pin the explicit ref")

	br, err := env.branch.GetBranch(ctx, wt.Branch)
	require.NoError(t, err)
	assert.Equal(t, earlier, br.HeadCommit)
	_ = gitstore.SyncState{}
}

// TestWorktreeAdd_ExplicitBaseByNameAndHEAD_Resolves verifies `--base` accepts
// an mgit branch NAME and the literal "HEAD" — not only a raw hex SHA — so the
// natural `mgit work --base main` invocation works (M1). Refs: MGIT-35, ADR-008 §4
func TestWorktreeAdd_ExplicitBaseByNameAndHEAD_Resolves(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	writeProjectFile(t, env, "a.go", "v1\n")
	require.NoError(t, env.wt.Add(ctx, "a.go"))
	c, err := env.commit.CreateCommit(ctx, CreateCommitRequest{
		TaskID: "MGIT-9.9", AgentID: "dev", Message: "base",
	})
	require.NoError(t, err)

	// The base branch (whatever the repo's default is) now tips at c.
	branchName, err := env.repo.CurrentBranch()
	require.NoError(t, err)
	mainBr, err := env.branch.GetBranch(ctx, branchName)
	require.NoError(t, err)
	require.Equal(t, c.CommitID, mainBr.HeadCommit)

	svc := newWorktreeSvcWithSync(env, "head-X")

	// --base by branch NAME.
	wtByName, err := svc.Add(ctx, model.WorktreeAddOptions{
		Path: filepath.Join(t.TempDir(), "byname"), TaskID: "MGIT-1.1", Base: branchName,
	})
	require.NoError(t, err)
	assert.Equal(t, c.CommitID, wtByName.ForkBase, "--base <branch> must resolve by name")

	// --base HEAD.
	wtByHead, err := svc.Add(ctx, model.WorktreeAddOptions{
		Path: filepath.Join(t.TempDir(), "byhead"), TaskID: "MGIT-1.2", Base: "HEAD",
	})
	require.NoError(t, err)
	head, err := env.repo.Head()
	require.NoError(t, err)
	assert.Equal(t, head, wtByHead.ForkBase, "--base HEAD must resolve to current HEAD")
}
