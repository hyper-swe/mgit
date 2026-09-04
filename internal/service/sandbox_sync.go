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
	sandboxID, booted, err := s.syncTarget(ctx, taskID)
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
// has booted.
//
// It waits out an in-flight boot of THAT task (awaitBootLocked) so the answer
// is settled rather than a snapshot of a sandbox mid-transition: reporting
// "not booted" for a VM that is seconds from running would tell the caller its
// host edits were skipped when the boot is about to stage them.
// Refs: FR-17.10, MGIT-122
func (s *SandboxService) syncTarget(ctx context.Context, taskID string) (sandboxID string, booted bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	reg, err := s.awaitBootLocked(ctx, taskID)
	if err != nil {
		return "", false, err
	}
	return reg.info.ID, reg.booted, nil
}

// VerifyGuestView asks the task's guest whether it reads what was last
// delivered to it. A sandbox that has not booted has had nothing delivered,
// and says so rather than passing. Refs: MGIT-164
func (s *SandboxService) VerifyGuestView(ctx context.Context, taskID string) (*model.GuestViewReport, error) {
	sandboxID, booted, err := s.syncTarget(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if !booted {
		return &model.GuestViewReport{Unverifiable: "nothing has been delivered to this sandbox yet: " + notBootedDetail}, nil
	}
	verifier, ok := s.manager.(model.GuestViewVerifier)
	if !ok {
		return nil, fmt.Errorf("%w: this sandbox's backend delivers the worktree as a launch-time "+
			"copy, so there is no later delivery to compare the guest against", model.ErrSandboxSyncUnsupported)
	}
	report, err := verifier.VerifyGuestView(ctx, sandboxID)
	if err != nil {
		return nil, fmt.Errorf("sandbox verify: %w", err)
	}
	return report, nil
}
