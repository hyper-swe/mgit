package service

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/hyper-swe/mgit/internal/model"
)

// deadGuest records why and when a sandbox's guest stopped answering.
// Refs: MGIT-99
type deadGuest struct {
	at    time.Time
	cause string
}

// markDeadLocked turns a reached-then-lost guest into a dead sandbox: the
// death is audited (guest_died, with the transport failure that showed it),
// recorded durably so the next daemon adopts it as dead, and remembered so
// every later exec is refused at once. The VM process and the registration
// stay — `remove` tears them down — and reg.booted stays true for exactly
// that reason: the teardown must stop the process that outlived its guest.
// Caller holds the lock. Refs: MGIT-99, FR-17.18, MGIT-102
func (s *SandboxService) markDeadLocked(ctx context.Context, reg *sandboxReg, cause error) error {
	detail, _ := json.Marshal(map[string]string{"cause": cause.Error()}) //nolint:errcheck // a map of strings always marshals
	if err := s.events.AppendSandboxEvent(ctx, &model.SandboxEvent{
		SandboxID: reg.info.ID, TaskID: reg.info.TaskID, EventType: model.EventGuestDied, Detail: string(detail),
	}); err != nil {
		return fmt.Errorf("record the guest's death: audit: %w", err)
	}
	if err := s.setPersistedState(ctx, reg.info.ID, model.StateDead); err != nil {
		return fmt.Errorf("record the guest's death: %w", err)
	}
	reg.info.State = model.StateDead
	reg.dead = &deadGuest{at: s.clock().UTC(), cause: cause.Error()}
	return nil
}

// noteExecFailure decides whether an exec failure means the guest is gone,
// and marks it so. Only a reached-then-lost signature counts — the caller's
// own cancellation and the host's own deadline say nothing about the guest —
// and only on a booted registration, which is the only kind that can be
// mid-command. Refs: MGIT-99, MGIT-118, MGIT-122
func (s *SandboxService) noteExecFailure(ctx context.Context, taskID string, execErr error) error {
	if ctx.Err() != nil || !model.IsLostServing(execErr) {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	reg, ok := s.byTask[taskID]
	if !ok || !reg.booted || reg.dead != nil {
		return nil
	}
	return s.markDeadLocked(ctx, reg, execErr)
}

// deadError is the refusal every command against a dead guest gets, at once
// and with the way out: the remedy keeps whatever the launch declared, and
// what the guest wrote since the last land or export is named as lost rather
// than left to be discovered. Refs: MGIT-99, MGIT-95
func deadError(reg *sandboxReg) error {
	relaunch := fmt.Sprintf("mgit sandbox launch --task-id %s --worktree %s --image %s",
		reg.info.TaskID, reg.info.WorktreePath, reg.opts.ImageRef)
	if reg.opts.MemoryMB > 0 {
		relaunch += fmt.Sprintf(" --memory-mb %d", reg.opts.MemoryMB)
	}
	if reg.opts.CPUs > 0 {
		relaunch += fmt.Sprintf(" --cpus %d", reg.opts.CPUs)
	}
	return fmt.Errorf("%w: sandbox %s (task %s) stopped answering at %s (%s); nothing runs in it any more. "+
		"Recover with `mgit sandbox remove %s --force` and then `%s` — what the guest wrote since your last "+
		"land or export is not reachable through mgit any more; if the workload needs more memory, "+
		"raise --memory-mb on that relaunch rather than reshaping the build",
		model.ErrGuestDead, reg.info.ID, reg.info.TaskID, reg.dead.at.Format(time.RFC3339), reg.dead.cause,
		reg.info.TaskID, relaunch)
}
