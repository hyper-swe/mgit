package service

import (
	"context"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// MGIT-123: the read-safe gate. `mgit status`/`mgit diff` used the same resync
// as `mgit work`, which staged the whole working tree into the base — so a
// user's uncommitted edit was absorbed into an untagged [mgit-sync] commit and
// became uncommittable to any task. The base may track only what the user's git
// has COMMITTED; uncommitted work stays uncommitted and visible.
// Refs: MGIT-123, ADR-008 §3

// gitBlobID returns the git blob id content would hash to, for building a fake
// "what git has committed" map.
func gitBlobID(content string) string {
	return plumbing.ComputeHash(plumbing.BlobObject, []byte(content)).String()
}

// newReadSyncService builds the read-verb sync gate with an injected view of
// what the project's git has committed (path -> blob id).
func newReadSyncService(env *testEnv, head string, committed map[string]string) *SyncService {
	return NewSyncService(env.repo, env.wt, env.cs, "", fixedClock()).
		withLocalReader(fakeLocal(head)).
		withCommittedReader(func(string) (map[string]string, error) { return committed, nil })
}

// TestEnsureSynced_UncommittedEdit_NotAbsorbedIntoBase is the MGIT-123 core: a
// file git has never committed must not enter the base on a read verb, and the
// base must not advance at all. Refs: MGIT-123, ADR-008 §3
func TestEnsureSynced_UncommittedEdit_NotAbsorbedIntoBase(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()
	base0, err := env.repo.Head()
	require.NoError(t, err)

	writeProjectFile(t, env, "only.txt", "content-B\n")
	// git has committed nothing at this path.
	require.NoError(t, newReadSyncService(env, "git-1", nil).EnsureSynced(ctx))

	base1, err := env.repo.Head()
	require.NoError(t, err)
	assert.Equal(t, base0, base1, "a read verb must not advance the base for an uncommitted edit")
	_, err = env.cs.GetFileFromCommit(ctx, base1, "only.txt")
	assert.Error(t, err, "uncommitted content must never be absorbed into the base")
}

// TestEnsureSynced_ModifiedTrackedFile_NotAbsorbed covers the modified-tracked
// case: the path is in git's tree but the working content differs, so it is
// uncommitted work and must stay out of the base. Refs: MGIT-123
func TestEnsureSynced_ModifiedTrackedFile_NotAbsorbed(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()
	base0, err := env.repo.Head()
	require.NoError(t, err)

	writeProjectFile(t, env, "a.go", "v2\n")
	committed := map[string]string{"a.go": gitBlobID("v1\n")} // git still holds v1
	require.NoError(t, newReadSyncService(env, "git-1", committed).EnsureSynced(ctx))

	base1, err := env.repo.Head()
	require.NoError(t, err)
	assert.Equal(t, base0, base1)
	_, err = env.cs.GetFileFromCommit(ctx, base1, "a.go")
	assert.Error(t, err, "a locally-modified tracked file is uncommitted work")
}

// TestEnsureSynced_GitHeadMoved_AbsorbsOnlyCommittedContent is the legitimate
// ADR-008 §3 resync: git's HEAD moved, so the base must take git's committed
// content — and ONLY that, leaving a separate in-flight edit alone.
// Refs: MGIT-123, ADR-008 §3
func TestEnsureSynced_GitHeadMoved_AbsorbsOnlyCommittedContent(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	writeProjectFile(t, env, "landed.go", "package landed\n")
	writeProjectFile(t, env, "wip.go", "package wip\n")
	committed := map[string]string{"landed.go": gitBlobID("package landed\n")}

	require.NoError(t, newReadSyncService(env, "git-moved", committed).EnsureSynced(ctx))

	head, err := env.repo.Head()
	require.NoError(t, err)
	got, err := env.cs.GetFileFromCommit(ctx, head, "landed.go")
	require.NoError(t, err, "git-committed content must still resync into the base")
	assert.Equal(t, "package landed\n", string(got))
	_, err = env.cs.GetFileFromCommit(ctx, head, "wip.go")
	assert.Error(t, err, "the in-flight edit must not ride along on a legitimate resync")
}

// TestEnsureSynced_UnchangedGitHead_CheapNoOp verifies the gate keys on git's
// COMMITTED head: once recorded, further working-tree edits do not re-enter the
// resync path at all. Refs: MGIT-123, ADR-008 §3
func TestEnsureSynced_UnchangedGitHead_CheapNoOp(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	svc := newReadSyncService(env, "git-1", map[string]string{"a.go": gitBlobID("v1\n")})
	writeProjectFile(t, env, "a.go", "v1\n")
	require.NoError(t, svc.EnsureSynced(ctx))
	base1, err := env.repo.Head()
	require.NoError(t, err)

	// Repeated reads over a drifting working tree must append nothing.
	for i := 0; i < 3; i++ {
		writeProjectFile(t, env, "a.go", "v2\n")
		require.NoError(t, svc.EnsureSynced(ctx))
	}
	base2, err := env.repo.Head()
	require.NoError(t, err)
	assert.Equal(t, base1, base2, "unchanged git HEAD → no base commit, however many reads")
}

// TestEnsureSynced_ReadPath_LeavesFoundationGateArmed protects the invariant
// behind the WorkTreeHash carry-forward: because the read path never absorbs the
// working tree, it must not record a fingerprint that would convince the
// new-worktree gate the foundation is already in the base — or `mgit work` would
// silently materialize without the developer's in-progress files (ADR-008 §2).
// Refs: MGIT-123, ADR-008 §2,§3
func TestEnsureSynced_ReadPath_LeavesFoundationGateArmed(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	writeProjectFile(t, env, "foundation.go", "package foundation\n")
	require.NoError(t, newReadSyncService(env, "git-1", nil).EnsureSynced(ctx))

	// A NEW worktree must still capture the uncommitted local foundation.
	require.NoError(t, newSyncService(env, "git-1", "").EnsureSyncedForNewWorktree(ctx))

	head, err := env.repo.Head()
	require.NoError(t, err)
	got, err := env.cs.GetFileFromCommit(ctx, head, "foundation.go")
	require.NoError(t, err, "the read path must not suppress the ADR-008 §2 foundation capture")
	assert.Equal(t, "package foundation\n", string(got))
}

// TestEnsureSynced_ReadPath_StagingNeutral verifies a read verb leaves the
// user's manual staging selection exactly as it found it. Refs: MGIT-123, ADR-008 §3
func TestEnsureSynced_ReadPath_StagingNeutral(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	writeProjectFile(t, env, "keep.go", "keep\n")
	require.NoError(t, env.wt.Add(ctx, "keep.go"))
	writeProjectFile(t, env, "landed.go", "package landed\n")

	committed := map[string]string{"landed.go": gitBlobID("package landed\n")}
	require.NoError(t, newReadSyncService(env, "git-moved", committed).EnsureSynced(ctx))

	staged, err := env.repo.StagedSnapshot()
	require.NoError(t, err)
	assert.Equal(t, []string{"keep.go"}, staged, "a read verb must not disturb staging")
}

// TestEnsureSynced_StagedTaskWIP_NeverAbsorbed keeps the MGIT-56 guarantee on
// the read path even when the staged content happens to match git's committed
// content: staged paths are pending task work and stay out of the base.
// Refs: MGIT-123, MGIT-56
func TestEnsureSynced_StagedTaskWIP_NeverAbsorbed(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	writeProjectFile(t, env, "wip.go", "package wip\n")
	require.NoError(t, env.wt.Add(ctx, "wip.go"))

	committed := map[string]string{"wip.go": gitBlobID("package wip\n")}
	require.NoError(t, newReadSyncService(env, "git-moved", committed).EnsureSynced(ctx))

	head, err := env.repo.Head()
	require.NoError(t, err)
	_, err = env.cs.GetFileFromCommit(ctx, head, "wip.go")
	assert.Error(t, err, "staged task WIP must not be absorbed, matching git or not")
}
