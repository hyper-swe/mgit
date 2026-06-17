package service

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/hyper-swe/mgit/internal/model"
)

// SandboxEventAppender records sandbox lifecycle events in the
// append-only audit trail (satisfied by internal/store/index.Store).
type SandboxEventAppender interface {
	AppendSandboxEvent(ctx context.Context, ev *model.SandboxEvent) error
}

// SandboxPolicyReader supplies the effective host policy (defaults +
// ceilings, FR-17.13), satisfied by internal/store/policy.Store.
type SandboxPolicyReader interface {
	Load(ctx context.Context) (model.SandboxPolicy, error)
}

// SandboxService is the lifecycle orchestrator: handlers go through it,
// never the manager or stores directly (architecture rule). It owns the
// sandbox ID, provisions lazily (register without booting; boot on first
// exec, FR-17.9/FR-17.10), enforces one-task/one-worktree exclusivity
// (FR-17.1, reusing FR-16's ErrTaskAlreadyBound), and audits every
// transition. Refs: FR-17.1, FR-17.9, FR-17.10
type SandboxService struct {
	manager model.SandboxManager
	events  SandboxEventAppender
	policy  SandboxPolicyReader
	clock   func() time.Time
	newID   func() (string, error)

	// byTask holds LIVE sandbox registrations, keyed by task ID. This is
	// in-memory by design, not a duplicate of the sandbox_events audit
	// trail: a microVM is a child of this daemon process, so a daemon
	// restart takes its VMs with it (microvm.Manager) — there is no live
	// sandbox to recover, and a fresh daemon correctly starts empty. The
	// DURABLE record (created/landed/destroyed) lives in sandbox_events
	// (DeriveState); the durable worktree<->task binding is FR-16's. The
	// daemon is single-instance (flock, MGIT-11.4), so the mutex
	// serializes exclusivity for live sandboxes.
	mu     sync.Mutex
	byTask map[string]*sandboxReg
}

// sandboxReg is one registered sandbox (booted or not).
type sandboxReg struct {
	info   model.SandboxInfo
	opts   model.SandboxLaunchOptions
	booted bool
}

// NewSandboxService wires the service. All dependencies are required
// (DI; no globals). newID assigns the host-owned sandbox ID.
func NewSandboxService(manager model.SandboxManager, events SandboxEventAppender, policy SandboxPolicyReader, clock func() time.Time, newID func() (string, error)) (*SandboxService, error) {
	switch {
	case manager == nil:
		return nil, fmt.Errorf("sandbox service: manager must not be nil")
	case events == nil:
		return nil, fmt.Errorf("sandbox service: event appender must not be nil")
	case policy == nil:
		return nil, fmt.Errorf("sandbox service: policy reader must not be nil")
	case clock == nil:
		return nil, fmt.Errorf("sandbox service: clock must not be nil")
	case newID == nil:
		return nil, fmt.Errorf("sandbox service: id generator must not be nil")
	}
	return &SandboxService{
		manager: manager, events: events, policy: policy,
		clock: clock, newID: newID, byTask: make(map[string]*sandboxReg),
	}, nil
}

// Register binds a sandbox to a task+worktree and records it WITHOUT
// booting the VM (lazy, FR-17.10). It rejects a second sandbox for the
// same task or worktree (FR-17.1, ErrTaskAlreadyBound). The created
// event is the registration audit (FR-17.18); the VM boots on the first
// EnsureRunning. Refs: FR-17.1, FR-17.10
func (s *SandboxService) Register(ctx context.Context, opts model.SandboxLaunchOptions) (*model.SandboxInfo, error) {
	if err := opts.Validate(); err != nil {
		return nil, fmt.Errorf("sandbox register: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkExclusivity(opts.TaskID, opts.WorktreePath); err != nil {
		return nil, err
	}

	id, err := s.newID()
	if err != nil {
		return nil, fmt.Errorf("sandbox register: %w", err)
	}
	opts.SandboxID = id
	digest := imageDigestOf(opts.ImageRef)

	// The created event is the registration audit and must succeed before
	// the sandbox is considered registered (no unaudited sandbox).
	if err := s.events.AppendSandboxEvent(ctx, &model.SandboxEvent{
		SandboxID: id, TaskID: opts.TaskID, EventType: model.EventCreated,
		ImageDigest: digest, NetworkMode: opts.Network.Mode,
	}); err != nil {
		return nil, fmt.Errorf("sandbox register: audit: %w", err)
	}

	info := model.SandboxInfo{
		ID: id, TaskID: opts.TaskID, WorktreePath: opts.WorktreePath,
		ImageDigest: digest, NetworkMode: opts.Network.Mode,
		NetworkAllowlist: opts.Network.Allowlist,
		State:            model.StateCreated, CreatedAt: s.clock().UTC(),
	}
	s.byTask[opts.TaskID] = &sandboxReg{info: info, opts: opts}
	return &info, nil
}

// EnsureRunning boots the task's sandbox if it is not already running
// (the first exec triggers this) and returns the running info. Policy
// defaults fill any unset resource limits. A boot failure leaves the
// registration intact (not booted) so the next attempt can retry.
// Refs: FR-17.9, FR-17.10
func (s *SandboxService) EnsureRunning(ctx context.Context, taskID string) (*model.SandboxInfo, error) {
	policy, err := s.policy.Load(ctx)
	if err != nil {
		return nil, fmt.Errorf("sandbox ensure-running: load policy: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	reg, ok := s.byTask[taskID]
	if !ok {
		return nil, fmt.Errorf("%w: task %q", model.ErrSandboxNotFound, taskID)
	}
	if reg.booted {
		info := reg.info
		return &info, nil
	}

	applyPolicyDefaults(&reg.opts, policy)
	launched, err := s.manager.Launch(ctx, reg.opts)
	if err != nil {
		return nil, fmt.Errorf("sandbox ensure-running: %w", err)
	}
	if err := s.events.AppendSandboxEvent(ctx, &model.SandboxEvent{
		SandboxID: launched.ID, TaskID: taskID, EventType: model.EventResumed,
	}); err != nil {
		return nil, fmt.Errorf("sandbox ensure-running: audit: %w", err)
	}
	reg.info = *launched
	reg.booted = true
	info := reg.info
	return &info, nil
}

// checkExclusivity rejects a duplicate task or worktree binding. Caller
// holds the lock. Refs: FR-17.1
func (s *SandboxService) checkExclusivity(taskID, worktreePath string) error {
	if _, exists := s.byTask[taskID]; exists {
		return fmt.Errorf("%w: task %q", model.ErrTaskAlreadyBound, taskID)
	}
	for _, reg := range s.byTask {
		if reg.opts.WorktreePath == worktreePath {
			return fmt.Errorf("%w: worktree %q", model.ErrTaskAlreadyBound, worktreePath)
		}
	}
	return nil
}

// applyPolicyDefaults fills unset (zero) resource limits from policy.
func applyPolicyDefaults(opts *model.SandboxLaunchOptions, p model.SandboxPolicy) {
	if opts.CPUs == 0 {
		opts.CPUs = p.CPUs
	}
	if opts.MemoryMB == 0 {
		opts.MemoryMB = p.MemoryMB
	}
	if opts.DiskQuotaMB == 0 {
		opts.DiskQuotaMB = p.DiskQuotaMB
	}
	if opts.TTL == 0 {
		opts.TTL = p.TTL
	}
}

// imageDigestOf extracts the sha256:<hex> digest from a pinned image
// reference (<name>@sha256:<hex>), already validated by opts.Validate.
func imageDigestOf(ref string) string {
	if _, digest, found := strings.Cut(ref, "@"); found {
		return digest
	}
	return ""
}
