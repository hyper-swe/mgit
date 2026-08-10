package service

import (
	"context"
	"fmt"

	"github.com/hyper-swe/mgit/internal/model"
)

// notBootedDetail explains the honest no-op a never-booted sandbox produces.
// It is stated once, here, because the CLI, MCP and any future caller all show
// it and it must not drift into three subtly different claims.
const notBootedDetail = "the sandbox has not booted yet; it stages the current worktree when it boots, " +
	"so there is nothing to propagate"

// SyncWorktree propagates the host worktree into a task's running sandbox, or
// — with DryRun — reports the classification without touching the guest.
//
// It adds no mechanism. The backend re-stages through the SAME host-side
// staging path a launch and the automatic pre-exec sync use, so this verb can
// never deliver something either of those would have refused (SEC-03). What it
// adds is CONTROL: re-staging without running anything in the guest, and — the
// part callers cannot get any other way — the conflict report, which until now
// was obtainable only by attempting work and being refused.
//
// A conflict returns BOTH the refusal error and the classification behind it,
// so naming the diverged paths costs no second round trip.
// Refs: MGIT-76, MGIT-71, FR-17.40, ADR-011
func (s *SandboxService) SyncWorktree(ctx context.Context, taskID string, opts model.WorktreeSyncOptions) (*model.WorktreeSyncReport, error) {
	sandboxID, booted, err := s.syncTarget(taskID)
	if err != nil {
		return nil, err
	}
	if !booted {
		// Lazy provisioning (FR-17.10): there is no guest tree yet, and boot
		// will stage the worktree as it is at that moment. Reported as a
		// genuine no-op WITH the reason — and never by booting a VM, which
		// would make asking a question expensive and surprising.
		return &model.WorktreeSyncReport{Skipped: true, DryRun: opts.DryRun, Detail: notBootedDetail}, nil
	}
	syncer, ok := s.manager.(model.WorktreeSyncer)
	if !ok {
		return nil, fmt.Errorf("%w: this sandbox's backend delivers the worktree as a launch-time "+
			"copy; re-launch the sandbox to pick up host changes", model.ErrSandboxSyncUnsupported)
	}
	report, err := syncer.SyncWorktree(ctx, sandboxID, opts)
	if err != nil {
		// The report (when the backend supplied one) is returned ALONGSIDE the
		// error: a refusal that cannot name what it refused is not actionable.
		return report, fmt.Errorf("sandbox sync: %w", err)
	}
	return report, nil
}

// syncTarget resolves a task to its host-owned sandbox ID and whether its VM
// has booted. Refs: FR-17.10
func (s *SandboxService) syncTarget(taskID string) (sandboxID string, booted bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	reg, ok := s.byTask[taskID]
	if !ok {
		return "", false, fmt.Errorf("%w: task %q", model.ErrSandboxNotFound, taskID)
	}
	return reg.info.ID, reg.booted, nil
}
