package microvm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/worktreesync"
)

// stagedTreeName is the per-VM staged worktree directory the virtiofs backends
// share into the guest. It is a shared convention rather than a per-backend
// secret: libkrun and vzf both stage here, which is what lets the manager
// propagate host changes without knowing which of them is running.
//
// A backend that delivers the worktree as a block image (firecracker) has no
// such directory, and stagedTreePath reports that by returning "".
// Refs: MGIT-71, ADR-005, ADR-011
const stagedTreeName = "worktree-staging"

// stagedTreePath returns the staged worktree directory for a sandbox, or ""
// when this backend does not deliver via a shared host directory.
func stagedTreePath(stateDir string) string {
	p := filepath.Join(stateDir, stagedTreeName)
	if info, err := os.Stat(p); err != nil || !info.IsDir() {
		return ""
	}
	return p
}

// ErrSyncUnsupported is returned when a backend cannot propagate host changes
// into a running guest.
//
// This is a real limitation, reported rather than papered over: firecracker
// delivers the worktree as an ext4 image built at launch and mounted by the
// guest, so the host cannot write into it without corrupting it. Such a
// sandbox keeps today's launch-time-copy semantics. Refs: MGIT-71, ADR-011
var ErrSyncUnsupported = errors.New("this sandbox backend cannot sync a running guest's worktree")

// SyncWorktree propagates host worktree changes into a running sandbox.
//
// It is the explicit verb; Exec also calls the same path automatically so an
// agent loop does not have to remember to. force overwrites paths the guest
// modified since delivery — each one is reported so it can be audited.
// Refs: MGIT-71
func (m *Manager) SyncWorktree(ctx context.Context, id string, force bool) (worktreesync.Result, error) {
	m.mu.Lock()
	sb, ok := m.sandboxes[id]
	m.mu.Unlock()
	if !ok {
		return worktreesync.Result{}, fmt.Errorf("%w: %q", model.ErrSandboxNotFound, id)
	}
	if sb.info.State != model.StateRunning {
		return worktreesync.Result{}, fmt.Errorf("%w: sandbox %q is %s, not running",
			model.ErrSandboxBackendUnavailable, id, sb.info.State)
	}
	return m.syncLocked(ctx, sb, force)
}

// syncLocked runs one sync under the sandbox's sync lock, which is also held
// across exec — so a command never observes a partially applied tree.
func (m *Manager) syncLocked(_ context.Context, sb *sandbox, force bool) (worktreesync.Result, error) {
	staged := stagedTreePath(sb.dir)
	if staged == "" {
		return worktreesync.Result{}, fmt.Errorf("%w: backend %q delivers the worktree as a "+
			"launch-time image; re-launch to pick up host changes",
			ErrSyncUnsupported, m.cfg.Backend)
	}
	if sb.info.WorktreePath == "" || sb.privateStore == "" {
		// A sandbox launched without a worktree has nothing to propagate.
		return worktreesync.Result{Skipped: true}, nil
	}

	sb.syncMu.Lock()
	defer sb.syncMu.Unlock()
	res, err := worktreesync.Sync(worktreesync.Request{
		WorktreePath:     sb.info.WorktreePath,
		PrivateStorePath: sb.privateStore,
		StateDir:         sb.dir,
		GuestTree:        staged,
		Force:            force,
	})
	if err != nil {
		return worktreesync.Result{}, err
	}
	if res.Changed() || len(res.Overridden) > 0 {
		// Audited at the manager because a sync mutates what the guest runs:
		// a reviewer reconstructing "what code did this sandbox execute"
		// needs every propagation, and every overwritten path, on the record.
		m.cfg.Logger.Info("worktree synced into the running sandbox",
			"event", "worktree_synced", "sandbox_id", sb.info.ID, "task_id", sb.info.TaskID,
			"updated", worktreesync.SortedPaths(res.Updated),
			"deleted", worktreesync.SortedPaths(res.Deleted),
			"overridden", worktreesync.SortedPaths(res.Overridden))
	}
	return res, nil
}

// syncBeforeExec propagates host changes ahead of a command, so a sandboxed
// agent loop tests the code the host actually has.
//
// A backend that cannot sync keeps its existing semantics rather than failing
// every exec. A CONFLICT, however, fails the exec loudly: running a command
// against knowingly stale code is the defect this exists to prevent, so
// "blocked, with the conflicting paths named" beats "ran, against the wrong
// tree". Refs: MGIT-71, ADR-011
func (m *Manager) syncBeforeExec(ctx context.Context, sb *sandbox) error {
	if stagedTreePath(sb.dir) == "" {
		return nil // launch-time-copy backend; unchanged behavior
	}
	_, err := m.syncLocked(ctx, sb, false)
	switch {
	case err == nil, errors.Is(err, ErrSyncUnsupported):
		return nil
	case errors.Is(err, worktreesync.ErrConflict):
		return fmt.Errorf("exec refused: the host worktree changed but %w", err)
	default:
		return fmt.Errorf("exec refused: could not sync the host worktree: %w", err)
	}
}
