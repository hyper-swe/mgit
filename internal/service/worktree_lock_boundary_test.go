package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/store/lock"
)

// WorktreeService.Add takes the repo-wide lock around its shared-store phase
// and releases it before materializing (MGIT-120). These tests pin the two ways
// that boundary can go wrong in code rather than under load: the lock must
// actually be released when Add returns (leak), and Add must never be entered
// while the caller already holds it (flock is per open file description, so a
// re-acquire inside the same process blocks on itself — a deadlock, not an
// error). Refs: MGIT-120, ADR-009, FR-16

// lockDir returns the store directory the Guarder locks for env's repo.
func lockDir(t *testing.T, env *testEnv) string {
	t.Helper()
	return filepath.Join(env.repo.Root(), ".mgit")
}

// assertLockFree proves no lock is outstanding by taking it with a wait so
// short that a held lock cannot be mistaken for a slow acquire.
func assertLockFree(t *testing.T, dir, when string) {
	t.Helper()
	fl, err := lock.Acquire(dir, 200*time.Millisecond)
	require.NoError(t, err, "the repo lock is still held %s", when)
	require.NoError(t, fl.Release())
}

// TestWorktreeService_Add_WithLocker_ReleasesTheLock proves the guarded claim
// leaves nothing behind: after Add returns, the lock is free, and a second Add
// through the same service succeeds (it must re-acquire, not re-enter).
// Refs: MGIT-120
func TestWorktreeService_Add_WithLocker_ReleasesTheLock(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()
	stageAndCommit(t, env, "MGIT-120.1", "src.go", "package src\n")

	dir := lockDir(t, env)
	assertLockFree(t, dir, "before Add")

	wtSvc := NewWorktreeService(env.idx, env.branch, env.wt, fixedClock()).
		WithLocker(lock.NewGuarder(dir, 2*time.Second))

	first := filepath.Join(t.TempDir(), "wt-a")
	wt, err := wtSvc.Add(ctx, model.WorktreeAddOptions{Path: first, TaskID: "MGIT-120.2"})
	require.NoError(t, err)
	assert.Equal(t, "task/MGIT-120.2", wt.Branch)
	assertLockFree(t, dir, "after a successful Add")

	second := filepath.Join(t.TempDir(), "wt-b")
	_, err = wtSvc.Add(ctx, model.WorktreeAddOptions{Path: second, TaskID: "MGIT-120.3"})
	require.NoError(t, err, "a second Add must re-acquire the lock, not deadlock on it")
	assertLockFree(t, dir, "after the second Add")
}

// TestWorktreeService_Add_WithLocker_RefusedClaim_ReleasesTheLock covers the
// error path: a claim refused by the FR-16 exclusivity constraints must still
// release the lock, or one refusal wedges the repo for every later command.
// Refs: MGIT-120, FR-16
func TestWorktreeService_Add_WithLocker_RefusedClaim_ReleasesTheLock(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()
	stageAndCommit(t, env, "MGIT-120.4", "src.go", "package src\n")

	dir := lockDir(t, env)
	wtSvc := NewWorktreeService(env.idx, env.branch, env.wt, fixedClock()).
		WithLocker(lock.NewGuarder(dir, 2*time.Second))

	_, err := wtSvc.Add(ctx, model.WorktreeAddOptions{
		Path: filepath.Join(t.TempDir(), "wt-a"), TaskID: "MGIT-120.5"})
	require.NoError(t, err)

	_, err = wtSvc.Add(ctx, model.WorktreeAddOptions{
		Path: filepath.Join(t.TempDir(), "wt-b"), TaskID: "MGIT-120.5"})
	require.ErrorIs(t, err, model.ErrTaskAlreadyBound)
	assertLockFree(t, dir, "after a refused Add")
}

// TestWorktreeService_Add_MaterializeFailure_ReleasesClaimAndLock proves the
// rollback path: when the unlocked materialization fails, the registration is
// removed (under the lock) so the task/branch/path are claimable again, and the
// lock is not left held. Refs: MGIT-120, FR-16
func TestWorktreeService_Add_MaterializeFailure_ReleasesClaimAndLock(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()
	stageAndCommit(t, env, "MGIT-120.6", "src.go", "package src\n")

	dir := lockDir(t, env)
	wtSvc := NewWorktreeService(env.idx, env.branch, env.wt, fixedClock()).
		WithLocker(lock.NewGuarder(dir, 2*time.Second))

	// A regular FILE where the worktree must go: the marker write cannot create
	// its directory there, so materialization fails after the claim is taken.
	blocked := filepath.Join(t.TempDir(), "occupied")
	require.NoError(t, os.WriteFile(blocked, []byte("in the way\n"), 0o600))

	_, err := wtSvc.Add(ctx, model.WorktreeAddOptions{Path: blocked, TaskID: "MGIT-120.7"})
	require.Error(t, err, "materialization onto a file must fail")
	assertLockFree(t, dir, "after a failed materialization")

	registered, err := wtSvc.List(ctx)
	require.NoError(t, err)
	assert.Empty(t, registered, "a failed provision must not leave a claim behind")

	// The task is claimable again at a usable path.
	_, err = wtSvc.Add(ctx, model.WorktreeAddOptions{
		Path: filepath.Join(t.TempDir(), "wt-retry"), TaskID: "MGIT-120.7"})
	require.NoError(t, err, "the released claim must be re-usable")
}

// TestWorktreeService_Add_NoLocker_StillWorks pins the pass-through contract:
// callers that already hold the lock for their whole operation (the MCP/REST
// middleware) wire no locker and must not be re-locked. Refs: MGIT-120, ADR-009
func TestWorktreeService_Add_NoLocker_StillWorks(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()
	stageAndCommit(t, env, "MGIT-120.8", "src.go", "package src\n")

	// Simulate the caller's outer lock: with no locker wired, Add must not try
	// to take it (which would block forever on our own flock).
	held, err := lock.Acquire(lockDir(t, env), 2*time.Second)
	require.NoError(t, err)
	defer func() { _ = held.Release() }()

	wtSvc := NewWorktreeService(env.idx, env.branch, env.wt, fixedClock())
	_, err = wtSvc.Add(ctx, model.WorktreeAddOptions{
		Path: filepath.Join(t.TempDir(), "wt"), TaskID: "MGIT-120.9"})
	require.NoError(t, err, "Add without a locker must run under the caller's lock")
}
