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

// PortPublishController opens and closes a sandbox's one-way host->guest
// published ports (SEC-09) around its lifecycle. It is an OPTIONAL
// collaborator wired at the daemon: StartPublish binds a 127.0.0.1 listener
// per requested port and forwards into the guest over the per-VM dialer;
// StopPublish closes every listener (no residue, FR-17.19). A sandbox with
// no published ports is a no-op. The host->guest direction only: there is no
// reverse path the guest could use to reach a host loopback service.
// Refs: SEC-09, FR-17.8, FR-17.19
type PortPublishController interface {
	StartPublish(ctx context.Context, info model.SandboxInfo, ports []model.PortPublish) error
	StopPublish(sandboxID string)
}

// CapabilityRevoker drops a sandbox's live capability grants on teardown so a
// grant never outlives the sandbox it was scoped to (satisfied by
// *CapabilityService). It is an OPTIONAL collaborator wired at the daemon.
// Refs: FR-17.12, SEC-05
type CapabilityRevoker interface {
	Revoke(sandboxID string)
}

// GrantReplayer re-applies a sandbox's live capability grants to the egress
// engine on resume (satisfied by *CapabilityService). Suspend keeps grants
// live but tears the egress proxy down; resume rebuilds an empty allowlist, so
// the held grants must be replayed or a granted destination is silently denied
// for the rest of the sandbox's life. OPTIONAL collaborator wired at the
// daemon. Refs: FR-17.12, SEC-05
type GrantReplayer interface {
	ReplayGrants(ctx context.Context, sandboxID string) error
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
	egress  EgressController      // optional; nil disables host egress orchestration
	capRev  CapabilityRevoker     // optional; nil disables capability-grant teardown
	capRep  GrantReplayer         // optional; nil disables grant replay on resume
	ports   PortPublishController // optional; nil disables one-way port publishing (SEC-09)

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

// sandboxReg is one registered sandbox (booted or not). lastActivity and
// expiresAt are host-clock timestamps driving the lifecycle sweeps
// (idle-suspend, TTL reap); they are owned by the service (not the backend)
// so the injected clock makes them deterministic in tests. Refs: NFR-17.3, FR-17.9
type sandboxReg struct {
	info         model.SandboxInfo
	opts         model.SandboxLaunchOptions
	booted       bool
	lastActivity time.Time // last boot/resume/exec; idle-suspend deadline runs from here
	expiresAt    time.Time // TTL deadline (registration time + TTL); zero = no TTL
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

// SetCapabilityRevoker wires the optional capability-grant revoker (the
// CapabilityService). Set once at daemon wiring time, before the service
// handles any request; nil leaves capability-grant teardown disabled. Kept off
// the constructor for the same reasons as SetEgressController. Refs: FR-17.12, SEC-05
func (s *SandboxService) SetCapabilityRevoker(c CapabilityRevoker) {
	s.capRev = c
}

// SetGrantReplayer wires the optional capability-grant replayer (the
// CapabilityService). Set once at daemon wiring time, before the service
// handles any request; nil leaves grant replay on resume disabled. Kept off
// the constructor for the same reasons as SetEgressController. Refs: FR-17.12, SEC-05
func (s *SandboxService) SetGrantReplayer(r GrantReplayer) {
	s.capRep = r
}

// SetPortPublishController wires the optional one-way port-publish controller
// (SEC-09). Set once at daemon wiring time, before the service handles any
// request; nil leaves port publishing disabled. Kept off the constructor for
// the same reasons as SetEgressController. Refs: SEC-09, FR-17.8
func (s *SandboxService) SetPortPublishController(c PortPublishController) {
	s.ports = c
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

	now := s.clock().UTC()
	info := model.SandboxInfo{
		ID: id, TaskID: opts.TaskID, WorktreePath: opts.WorktreePath,
		ImageDigest: digest, NetworkMode: opts.Network.Mode,
		NetworkAllowlist: opts.Network.Allowlist,
		PublishPorts:     opts.PublishPorts,
		State:            model.StateCreated, CreatedAt: now,
	}
	// lastActivity seeds the idle-suspend deadline from registration time;
	// expiresAt is resolved at boot (with the effective TTL) or lazily in the
	// reap sweep for a never-booted sandbox. Refs: NFR-17.3, FR-17.9
	s.byTask[opts.TaskID] = &sandboxReg{info: info, opts: opts, lastActivity: now}
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
		// Resume rebuilds a fresh, EMPTY egress proxy. Replay any live
		// capability grants into it so a destination granted before suspend
		// stays admitted — otherwise it is silently denied for the rest of the
		// sandbox's life while RecordDenial still treats it as live and
		// suppresses re-prompting (F-D). On first boot the grant set is empty,
		// so this is a no-op. A replay failure fails closed (roll the VM and its
		// egress back); the registration stays un-booted and retryable.
		// Refs: FR-17.12, SEC-05
		if s.capRep != nil {
			if repErr := s.capRep.ReplayGrants(ctx, launched.ID); repErr != nil {
				s.egress.StopEgress(launched.ID)
				return nil, errors.Join(
					fmt.Errorf("sandbox ensure-running: grant replay: %w", repErr),
					s.manager.Stop(ctx, launched.ID, true),
					s.manager.Remove(ctx, launched.ID, true),
				)
			}
		}
	}
	// Open the one-way published ports (SEC-09) once the VM and its egress are
	// up: each binds a 127.0.0.1 host listener forwarding INTO the guest. A
	// bind failure fails the boot closed (roll the VM and its egress back) so
	// no half-published sandbox runs; the registration stays un-booted and
	// retryable. None requested is a no-op. Refs: SEC-09, FR-17.8
	if s.ports != nil && len(reg.opts.PublishPorts) > 0 {
		if pubErr := s.ports.StartPublish(ctx, *launched, reg.opts.PublishPorts); pubErr != nil {
			if s.egress != nil {
				s.egress.StopEgress(launched.ID)
			}
			return nil, errors.Join(
				fmt.Errorf("sandbox ensure-running: publish ports: %w", pubErr),
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
		if s.ports != nil {
			s.ports.StopPublish(launched.ID)
		}
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
	// The backend's SandboxInfo does not carry the published-port mappings
	// (they are a service-level concern); restore them so List/Status keep
	// reporting them after boot. Refs: SEC-09
	reg.info.PublishPorts = reg.opts.PublishPorts
	reg.booted = true
	// Record activity and the TTL deadline from the service clock (not the
	// backend's), so idle-suspend and TTL reap are deterministic. The
	// effective TTL was filled into opts by applyPolicyDefaults above.
	// Refs: NFR-17.3, FR-17.9
	now := s.clock().UTC()
	reg.lastActivity = now
	if reg.opts.TTL > 0 {
		reg.expiresAt = now.Add(reg.opts.TTL)
		reg.info.ExpiresAt = reg.expiresAt
	}
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
	// A completed exec is activity: reset the idle-suspend deadline so an
	// actively-used sandbox is never suspended out from under its agent.
	// Refs: NFR-17.3
	s.mu.Lock()
	if reg, ok := s.byTask[taskID]; ok {
		reg.lastActivity = s.clock().UTC()
	}
	s.mu.Unlock()
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
	if err := s.teardownLocked(ctx, reg, model.EventDestroyed, force); err != nil {
		return err
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
