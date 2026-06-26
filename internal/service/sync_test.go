package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gitstore "github.com/hyper-swe/mgit/internal/store/git"
	"github.com/hyper-swe/mgit/internal/store/gitref"
)

// fakeLocal returns a localStateReader that always reports the given commit id,
// so the resync logic can be exercised without a real `.git`.
func fakeLocal(head string) localStateReader {
	return func(string) (*gitref.LocalState, error) {
		return &gitref.LocalState{HeadCommit: head, Ref: "refs/heads/main"}, nil
	}
}

func newSyncService(env *testEnv, head, boundTask string) *SyncService {
	return NewSyncService(env.repo, env.wt, env.cs, boundTask, fixedClock()).
		withLocalReader(fakeLocal(head))
}

func writeProjectFile(t *testing.T, env *testEnv, rel, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(env.repo.Root(), rel), []byte(content), 0o600))
}

// TestEnsureSynced_NewWorktreeCarriesLocalFoundation is the executable spec for
// ADR-008 §2: a resync captures the current LOCAL working state (incl. files
// never committed to .mgit), so a worktree materialized AFTER it carries the
// developer's unpushed foundation. Refs: MGIT-35, ADR-008 §2
func TestEnsureSynced_NewWorktreeCarriesLocalFoundation(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	// Local foundation present on disk but not yet in .mgit.
	writeProjectFile(t, env, "foundation.go", "package foundation\n")

	svc := newSyncService(env, "aaaa", "")
	require.NoError(t, svc.EnsureSynced(ctx))

	// The base now contains the foundation file.
	head, err := env.repo.Head()
	require.NoError(t, err)
	got, err := env.cs.GetFileFromCommit(ctx, head, "foundation.go")
	require.NoError(t, err)
	assert.Equal(t, "package foundation\n", string(got))
}

// TestEnsureSynced_DriftAfterChange_AutoResyncs verifies a content change is
// auto-detected and resynced. Refs: MGIT-35, ADR-008 §3
func TestEnsureSynced_DriftAfterChange_AutoResyncs(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	writeProjectFile(t, env, "a.go", "v1\n")
	svc := newSyncService(env, "head1", "")
	require.NoError(t, svc.EnsureSynced(ctx))
	base1, err := env.repo.Head()
	require.NoError(t, err)

	// Same-size content change (would defeat an mtime/size signal) → must still
	// be detected by the content fingerprint.
	writeProjectFile(t, env, "a.go", "v2\n")
	require.NoError(t, svc.EnsureSynced(ctx))
	base2, err := env.repo.Head()
	require.NoError(t, err)

	assert.NotEqual(t, base1, base2, "drift must advance the base")
	got, err := env.cs.GetFileFromCommit(ctx, base2, "a.go")
	require.NoError(t, err)
	assert.Equal(t, "v2\n", string(got))
}

// TestEnsureSynced_CleanState_CheapNoOp verifies the common path appends no
// commit when nothing changed. Refs: MGIT-35, ADR-008 §3
func TestEnsureSynced_CleanState_CheapNoOp(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	writeProjectFile(t, env, "a.go", "stable\n")
	svc := newSyncService(env, "h", "")
	require.NoError(t, svc.EnsureSynced(ctx))
	base1, err := env.repo.Head()
	require.NoError(t, err)

	// Second call with no change: base must be byte-identical (no empty commit).
	require.NoError(t, svc.EnsureSynced(ctx))
	base2, err := env.repo.Head()
	require.NoError(t, err)
	assert.Equal(t, base1, base2, "clean re-sync must be a no-op")
}

// TestEnsureSynced_InWorktree_NeverResyncs verifies a bound worktree's pinned
// fork-base is never shifted by the gate. Refs: MGIT-35, ADR-008 §3
func TestEnsureSynced_InWorktree_NeverResyncs(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	base0, err := env.repo.Head()
	require.NoError(t, err)
	// A drift exists on disk, but boundTask != "" must short-circuit.
	writeProjectFile(t, env, "drift.go", "x\n")
	svc := newSyncService(env, "anything", "MGIT-1.1")
	require.NoError(t, svc.EnsureSynced(ctx))

	base1, err := env.repo.Head()
	require.NoError(t, err)
	assert.Equal(t, base0, base1, "worktree gate must not resync")
}

// TestEnsureSynced_NoGit_Degrades verifies the gate degrades (uses the .mgit
// base as-is) when the project has no readable git. Refs: MGIT-35, ADR-008 §6
func TestEnsureSynced_NoGit_Degrades(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()
	base0, err := env.repo.Head()
	require.NoError(t, err)

	svc := NewSyncService(env.repo, env.wt, env.cs, "", fixedClock()).
		withLocalReader(func(string) (*gitref.LocalState, error) {
			return nil, gitref.ErrNoGit
		})
	require.NoError(t, svc.EnsureSynced(ctx))

	base1, err := env.repo.Head()
	require.NoError(t, err)
	assert.Equal(t, base0, base1)
}

// TestEnsureSynced_UnreadableGit_FailsLoud verifies an unsupported git state is
// NOT silently ignored — the gate fails loud rather than risk a stale base.
// Refs: MGIT-35, ADR-008 §6
func TestEnsureSynced_UnreadableGit_FailsLoud(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	svc := NewSyncService(env.repo, env.wt, env.cs, "", fixedClock()).
		withLocalReader(func(string) (*gitref.LocalState, error) {
			return nil, gitref.ErrUnsupportedGitState
		})
	err := svc.EnsureSynced(ctx)
	require.Error(t, err)
	assert.True(t, errors.Is(err, gitref.ErrUnsupportedGitState))
}

// TestEnsureSynced_DriftWithManualStaging_PreservesStaging verifies the resync
// is STAGING-NEUTRAL: the gate fires on read-ish commands (`mgit status`/`diff`),
// so a user's manual partial staging selection on the shared host store must
// survive a drift resync rather than be consumed/cleared. Refs: MGIT-35, ADR-008 §3
func TestEnsureSynced_DriftWithManualStaging_PreservesStaging(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	// A manual partial staging selection.
	writeProjectFile(t, env, "keep.go", "keep\n")
	require.NoError(t, env.wt.Add(ctx, "keep.go"))
	stagedBefore, err := env.repo.StagedSnapshot()
	require.NoError(t, err)
	require.Equal(t, []string{"keep.go"}, stagedBefore)

	// A second file drifts the working tree; EnsureSynced (as on `mgit status`)
	// resyncs the base — and must restore the user's staging selection after.
	writeProjectFile(t, env, "other.go", "other\n")
	require.NoError(t, newSyncService(env, "git-Z", "").EnsureSynced(ctx))

	stagedAfter, err := env.repo.StagedSnapshot()
	require.NoError(t, err)
	assert.Equal(t, stagedBefore, stagedAfter, "resync must not destroy a manual staging selection")
}

// TestEnsureSynced_InterruptedResync_LeavesConsistentBase verifies that if the
// drift signal is lost (e.g. an interrupt after the base advanced but before the
// state file was written), a re-run converges to an identical base rather than
// piling on commits or tearing the base. Refs: MGIT-35, ADR-008 §6
func TestEnsureSynced_InterruptedResync_LeavesConsistentBase(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	writeProjectFile(t, env, "a.go", "content\n")
	svc := newSyncService(env, "h", "")
	require.NoError(t, svc.EnsureSynced(ctx))
	base1, err := env.repo.Head()
	require.NoError(t, err)

	// Simulate an interrupt that dropped the persisted drift signal.
	require.NoError(t, os.Remove(filepath.Join(env.repo.MgitDir(), "sync_state.json")))

	require.NoError(t, svc.EnsureSynced(ctx))
	base2, err := env.repo.Head()
	require.NoError(t, err)

	// The re-run sees no real content change vs the base, so it must NOT append
	// a second commit — the base is consistent and identical.
	assert.Equal(t, base1, base2, "re-run after lost signal must converge, not pile commits")
	_ = gitstore.SyncState{} // touch import
}
