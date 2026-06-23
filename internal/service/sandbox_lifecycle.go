package service

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/hyper-swe/mgit/internal/model"
)

// SuspendIdle pauses every booted sandbox idle past the threshold and
// returns the task IDs suspended. Suspend is a graceful pause (Stop with
// force=false): the VM drops to zero CPU (NFR-17.3) but its disk and
// registration survive, so the next exec resumes it (EnsureRunning re-boots
// the un-booted registration, audited as resumed). The host clock — not the
// guest — decides idleness (SEC-05). Every suspend is audited; the first
// failure stops the sweep and surfaces so it is retried next pass.
// Refs: NFR-17.3, FR-17.18
func (s *SandboxService) SuspendIdle(ctx context.Context, idleThreshold time.Duration) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock().UTC()
	suspended := make([]string, 0)
	for _, taskID := range s.sortedTasksLocked() {
		reg := s.byTask[taskID]
		if !reg.booted || now.Sub(reg.lastActivity) < idleThreshold {
			continue
		}
		if err := s.suspendLocked(ctx, reg); err != nil {
			return suspended, err
		}
		suspended = append(suspended, taskID)
	}
	return suspended, nil
}

// suspendLocked pauses one booted sandbox and audits the transition. The VM
// is paused before the audit (the dangerous direction — a running VM —
// closes first); leaving it un-booted makes the next exec resume it. Caller
// holds the lock. Refs: NFR-17.3, FR-17.18
func (s *SandboxService) suspendLocked(ctx context.Context, reg *sandboxReg) error {
	// Suspend releases the host listeners with the VM; resume re-opens them in
	// EnsureRunning (the un-booted registration re-boots and re-publishes from
	// reg.opts.PublishPorts). Refs: SEC-09, NFR-17.3
	if s.ports != nil {
		s.ports.StopPublish(reg.info.ID)
	}
	if s.egress != nil {
		s.egress.StopEgress(reg.info.ID)
	}
	// force=false: idle-suspend is a graceful pause, not a kill.
	if err := s.manager.Stop(ctx, reg.info.ID, false); err != nil {
		return fmt.Errorf("sandbox suspend: stop: %w", err)
	}
	if err := s.events.AppendSandboxEvent(ctx, &model.SandboxEvent{
		SandboxID: reg.info.ID, TaskID: reg.info.TaskID, EventType: model.EventSuspended,
	}); err != nil {
		return fmt.Errorf("sandbox suspend: audit: %w", err)
	}
	reg.booted = false
	reg.info.State = model.StateSuspended
	return nil
}

// ReapExpired tears down every sandbox past its TTL deadline and returns
// the task IDs reaped, emitting a ttl_expired audit event for each
// (FR-17.9). The deadline is the host-clock launch time plus the effective
// TTL; a never-booted sandbox is reaped against its registration time plus
// the policy TTL. A zero TTL means no deadline (never reaped). The first
// teardown failure stops the sweep and surfaces for the next pass.
// Refs: FR-17.9, FR-17.18
func (s *SandboxService) ReapExpired(ctx context.Context) ([]string, error) {
	return s.reapExpired(ctx, model.EventTTLExpired)
}

// PruneAbandoned reaps every sandbox past its TTL across all tasks — the
// abandoned-sandbox sweep (FR-17.2). It shares ReapExpired's deadline logic
// (an abandoned sandbox is one whose TTL has elapsed with no land) and
// audits each as ttl_expired so the durable trail records why it was torn
// down. Refs: FR-17.2, FR-17.9, FR-17.18
func (s *SandboxService) PruneAbandoned(ctx context.Context) ([]string, error) {
	return s.reapExpired(ctx, model.EventTTLExpired)
}

// reapExpired tears down every sandbox whose TTL deadline has passed,
// auditing each with the given terminal event type. Refs: FR-17.9, FR-17.18
func (s *SandboxService) reapExpired(ctx context.Context, eventType string) ([]string, error) {
	policy, err := s.policy.Load(ctx)
	if err != nil {
		return nil, fmt.Errorf("sandbox reap: load policy: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock().UTC()
	reaped := make([]string, 0)
	for _, taskID := range s.sortedTasksLocked() {
		reg := s.byTask[taskID]
		if !s.expiredLocked(reg, policy, now) {
			continue
		}
		if err := s.teardownLocked(ctx, reg, eventType, true); err != nil {
			return reaped, err
		}
		delete(s.byTask, taskID)
		reaped = append(reaped, taskID)
	}
	return reaped, nil
}

// expiredLocked reports whether a registration is past its TTL deadline.
// A booted sandbox uses its recorded expiresAt; a never-booted one uses its
// registration time plus the policy TTL (its boot would have used the same
// effective TTL). A zero effective TTL means no deadline. Caller holds the
// lock. Refs: FR-17.9
func (s *SandboxService) expiredLocked(reg *sandboxReg, policy model.SandboxPolicy, now time.Time) bool {
	deadline := reg.expiresAt
	if deadline.IsZero() {
		ttl := reg.opts.TTL
		if ttl == 0 {
			ttl = policy.TTL
		}
		if ttl == 0 {
			return false // no TTL configured: never reaped
		}
		deadline = reg.info.CreatedAt.Add(ttl)
	}
	return !now.Before(deadline)
}

// Land records a verified land for a task's sandbox and then tears it down:
// land is the success exit of a sandbox's life (FR-17.5). The landed event
// is appended FIRST (and must succeed before any VM is destroyed — an
// unrecorded land must not silently discard the sandbox), then the VM is
// stopped, removed, and the destroyed transition audited, freeing the
// task+worktree binding. Refs: FR-17.5, FR-17.9, FR-17.18
func (s *SandboxService) Land(ctx context.Context, taskID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	reg, ok := s.byTask[taskID]
	if !ok {
		return fmt.Errorf("%w: task %q", model.ErrSandboxNotFound, taskID)
	}
	if err := s.events.AppendSandboxEvent(ctx, &model.SandboxEvent{
		SandboxID: reg.info.ID, TaskID: taskID, EventType: model.EventLanded,
	}); err != nil {
		return fmt.Errorf("sandbox land: audit: %w", err)
	}
	reg.info.State = model.StateLanded
	if err := s.teardownLocked(ctx, reg, model.EventDestroyed, false); err != nil {
		return err
	}
	delete(s.byTask, taskID)
	return nil
}

// teardownLocked stops and removes a booted sandbox's VM (force as given),
// then appends the terminal lifecycle event. An un-booted sandbox has no VM
// to touch — only the audit event is written. The VM is destroyed before
// the audit so the dangerous direction (a stranded VM) closes first; a
// backend or audit failure leaves the registration intact for the caller to
// retry. Caller holds the lock and removes the registration on success.
// Refs: FR-17.9, FR-17.18
func (s *SandboxService) teardownLocked(ctx context.Context, reg *sandboxReg, eventType string, force bool) error {
	if reg.booted {
		// Revoke any live capability grants and stop host egress first, so the
		// grants die with the sandbox (scoped to its lifetime, SEC-05) and the
		// proxy/DNS listeners are released before the VM and its tap go away.
		// This fires on every teardown path (Remove, Land, TTL/idle reap).
		// Refs: FR-17.12, SEC-05
		if s.capRev != nil {
			s.capRev.Revoke(reg.info.ID)
		}
		// Close the one-way published ports BEFORE the VM goes away so no host
		// 127.0.0.1 listener outlives the sandbox it forwarded into (no residue,
		// FR-17.19). Idempotent and nil-safe. Refs: SEC-09, FR-17.19
		if s.ports != nil {
			s.ports.StopPublish(reg.info.ID)
		}
		if s.egress != nil {
			s.egress.StopEgress(reg.info.ID)
		}
		if err := s.manager.Stop(ctx, reg.info.ID, force); err != nil {
			return fmt.Errorf("sandbox teardown: stop: %w", err)
		}
		if err := s.manager.Remove(ctx, reg.info.ID, force); err != nil {
			return fmt.Errorf("sandbox teardown: %w", err)
		}
	}
	if err := s.events.AppendSandboxEvent(ctx, &model.SandboxEvent{
		SandboxID: reg.info.ID, TaskID: reg.info.TaskID, EventType: eventType,
	}); err != nil {
		return fmt.Errorf("sandbox teardown: audit: %w", err)
	}
	return nil
}

// sortedTasksLocked returns the registered task IDs in a stable order so
// the lifecycle sweeps are deterministic. Caller holds the lock.
func (s *SandboxService) sortedTasksLocked() []string {
	tasks := make([]string, 0, len(s.byTask))
	for taskID := range s.byTask {
		tasks = append(tasks, taskID)
	}
	sort.Strings(tasks)
	return tasks
}
