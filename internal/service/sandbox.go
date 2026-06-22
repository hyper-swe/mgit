package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
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

// EgressController starts and stops a sandbox's host-side network
// enforcement (the allowlist proxy + restricted DNS) around its lifecycle.
// It is an OPTIONAL collaborator wired at the daemon: StartEgress is a no-op
// for none/open sandboxes (they run no proxy) and for backends without a
// host tap. The implementation (over egress.Runner) derives the per-sandbox
// gateway the proxy/DNS bind from the launched SandboxInfo. The service
// stays backend-agnostic: it only signals the boot/teardown transitions.
// Refs: FR-17.7, FR-17.8, SEC-04
type EgressController interface {
	StartEgress(ctx context.Context, info model.SandboxInfo) error
	StopEgress(sandboxID string)
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
	egress  EgressController // optional; nil disables host egress orchestration

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

// SetEgressController wires the optional host egress controller (the
// allowlist proxy + DNS lifecycle). It is set once at daemon wiring time,
// before the service handles any request; nil leaves egress orchestration
// disabled. Kept off the constructor to respect the parameter-count limit
// and because it is an optional collaborator. Refs: FR-17.8
func (s *SandboxService) SetEgressController(c EgressController) {
	s.egress = c
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
	// the sandbox is considered registered (no unaudited sandbox). Open
	// network mode carries a recorded risk note (T3/T9 disabled) so the
	// user-accepted risk is permanently attributable (FR-17.7).
	created := &model.SandboxEvent{
		SandboxID: id, TaskID: opts.TaskID, EventType: model.EventCreated,
		ImageDigest: digest, NetworkMode: opts.Network.Mode,
	}
	if note, risky := model.NetworkRiskNote(opts.Network.Mode); risky {
		created.Detail = fmt.Sprintf(`{"network_risk":%q}`, note)
	}
	if err := s.events.AppendSandboxEvent(ctx, created); err != nil {
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
	// Start host egress enforcement (the allowlist proxy + DNS) before the
	// sandbox is considered up, so an allowlist guest never runs without its
	// host-side controls. A failure rolls the VM back (fail closed); none/
	// open are no-ops in the controller. Refs: FR-17.7, FR-17.8, SEC-04
	if s.egress != nil {
		if egErr := s.egress.StartEgress(ctx, *launched); egErr != nil {
			return nil, errors.Join(
				fmt.Errorf("sandbox ensure-running: egress: %w", egErr),
				s.manager.Stop(ctx, launched.ID, true),
				s.manager.Remove(ctx, launched.ID, true),
			)
		}
	}
	if auditErr := s.events.AppendSandboxEvent(ctx, &model.SandboxEvent{
		SandboxID: launched.ID, TaskID: taskID, EventType: model.EventResumed,
	}); auditErr != nil {
		// A booted-but-unaudited VM must never survive (an unaudited
		// sandbox is an audit-trail gap, FR-17.18). Roll back the VM we
		// just launched (and its egress) before returning; the registration
		// stays un-booted and retryable. Errors are joined so none is swallowed.
		if s.egress != nil {
			s.egress.StopEgress(launched.ID)
		}
		return nil, errors.Join(
			fmt.Errorf("sandbox ensure-running: audit: %w", auditErr),
			s.manager.Stop(ctx, launched.ID, true),
			s.manager.Remove(ctx, launched.ID, true),
		)
	}
	reg.info = *launched
	reg.booted = true
	info := reg.info
	return &info, nil
}

// Exec routes one command into the task's sandbox, booting it on first
// use (EnsureRunning, lazy provisioning) and returning the guest's result
// unchanged. Handlers call this, never the manager directly (architecture
// rule). The manager owns the transport into the guest. Refs: FR-17.9, FR-17.11
func (s *SandboxService) Exec(ctx context.Context, taskID string, req model.ExecRequest) (*model.ExecResult, error) {
	info, err := s.EnsureRunning(ctx, taskID)
	if err != nil {
		return nil, err
	}
	// A positive per-exec timeout bounds this single command (the
	// ExecRequest.Timeout contract; zero leaves the sandbox TTL to govern).
	// Deriving it here — after the boot, which the timeout must not bound —
	// makes both backends enforce it through the request context (the
	// microVM conn deadline, the container exec context). Refs: FR-17.11
	if req.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}
	res, err := s.manager.Exec(ctx, info.ID, req)
	if err != nil {
		return nil, fmt.Errorf("sandbox exec: %w", err)
	}
	return res, nil
}

// List returns every registered sandbox (created and running), sorted by
// task ID for stable output. The registry — not the backend — is the
// source of truth here: it includes lazily-created sandboxes the backend
// has not yet booted. Refs: FR-17.9, FR-17.18
func (s *SandboxService) List(_ context.Context) ([]model.SandboxInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]model.SandboxInfo, 0, len(s.byTask))
	for _, reg := range s.byTask {
		out = append(out, reg.info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TaskID < out[j].TaskID })
	return out, nil
}

// Status returns the registered sandbox bound to a task, or
// ErrSandboxNotFound. Refs: FR-17.9
func (s *SandboxService) Status(_ context.Context, taskID string) (*model.SandboxInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	reg, ok := s.byTask[taskID]
	if !ok {
		return nil, fmt.Errorf("%w: task %q", model.ErrSandboxNotFound, taskID)
	}
	info := reg.info
	return &info, nil
}

// Remove tears down a task's sandbox and frees its task+worktree binding.
// A booted VM is stopped and removed first (the dangerous direction — a
// stranded running VM — is closed before the audit), then a destroyed
// event is appended, then the registration is dropped. A backend or audit
// failure leaves the sandbox registered and retryable. Refs: FR-17.9, FR-17.18
func (s *SandboxService) Remove(ctx context.Context, taskID string, force bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	reg, ok := s.byTask[taskID]
	if !ok {
		return fmt.Errorf("%w: task %q", model.ErrSandboxNotFound, taskID)
	}
	if reg.booted {
		// Stop host egress first so its proxy/DNS listeners are released
		// before the VM and its tap go away (best-effort; teardown proceeds).
		if s.egress != nil {
			s.egress.StopEgress(reg.info.ID)
		}
		if err := s.manager.Stop(ctx, reg.info.ID, force); err != nil {
			return fmt.Errorf("sandbox remove: stop: %w", err)
		}
		if err := s.manager.Remove(ctx, reg.info.ID, force); err != nil {
			return fmt.Errorf("sandbox remove: %w", err)
		}
	}
	if err := s.events.AppendSandboxEvent(ctx, &model.SandboxEvent{
		SandboxID: reg.info.ID, TaskID: taskID, EventType: model.EventDestroyed,
	}); err != nil {
		return fmt.Errorf("sandbox remove: audit: %w", err)
	}
	delete(s.byTask, taskID)
	return nil
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
