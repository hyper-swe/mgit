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

// SandboxRegistry is the DURABLE roster of registered sandboxes (satisfied by
// internal/store/index.Store). It is what makes a registration outlive the
// daemon process that created it: registration is lazy (FR-17.9, FR-17.10),
// so a never-booted sandbox holds no VM to keep its daemon alive, the daemon
// idle-exits (NFR-17.6), and without this the registration went with it —
// `mgit sandbox status` answering "sandbox not found" for containment the
// agent had been told it had (MGIT-102).
//
// It is live state, NOT the audit trail: SandboxEventAppender remains the
// append-only history, and every transition recorded here is also recorded
// there. Refs: FR-17.1, FR-17.9, FR-17.18, MGIT-102
type SandboxRegistry interface {
	UpsertSandbox(ctx context.Context, reg *model.SandboxRegistration) error
	SetSandboxState(ctx context.Context, sandboxID, state string) error
	DeleteSandbox(ctx context.Context, sandboxID string) error
	ListSandboxes(ctx context.Context) ([]model.SandboxRegistration, error)
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
	// registry is the DURABLE roster (MGIT-102). Nil leaves the service
	// memory-only — the pre-MGIT-102 behavior, kept for wirings that have no
	// index (unit tests, greet-only daemons), never for a serving daemon.
	registry SandboxRegistry

	// byTask is the in-process WORKING SET of registrations, keyed by task ID:
	// a fast, lock-guarded view of the durable rows in `registry`, plus the
	// per-process boot state (a VM handle cannot be persisted).
	//
	// It used to be the ONLY record, on the reasoning that a microVM is a
	// child of this process so a restart takes its VMs with it and a fresh
	// daemon correctly starts empty. That reasoning missed the state a
	// registration is normally in: lazy provisioning (FR-17.9, FR-17.10)
	// registers WITHOUT booting, so the most common sandbox is one with no VM
	// to lose — and losing its registration cost containment availability
	// exactly when an agent had been told it was contained (MGIT-102).
	// Rehydrate rebuilds this map from `registry`, verifying rather than
	// assuming any state that claims a VM exists.
	//
	// The daemon is single-instance (flock, MGIT-11.4), so the mutex
	// serializes exclusivity for live sandboxes.
	//
	// IT IS HELD FOR STATE TRANSITIONS, NEVER FOR A BOOT. It used to be held
	// across manager.Launch, and a boot is the longest thing this daemon does:
	// 16.5 s at four concurrent sandboxes and 41-59 s at eight
	// (docs/E2E-MATRIX.md). Every other sandbox's exec, status and removal
	// queued behind it and expired on the client's socket deadline, reported to
	// the agent as "the guest stopped answering" about guests that were fine.
	// The boot now runs OUTSIDE this lock; see EnsureRunning for the claim that
	// replaces it and awaitBootLocked for how teardown stays safe.
	// Refs: MGIT-122, ADR-009 (amendment), NFR-17.6
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
	// boot is non-nil for exactly as long as a boot is in flight for this
	// registration. It is the claim that replaces "the service mutex is held
	// across Launch": set under the lock before the lock is dropped, cleared
	// under the lock when the boot settles. Refs: MGIT-122
	boot *bootAttempt
}

// bootAttempt is one in-flight VM boot, shared by every caller that arrives for
// the same task while it runs.
//
// It carries the whole outcome, not just a signal, for two reasons. A caller
// that merely waited and then re-read the registration could observe a boot
// that failed and start its own — N callers against a broken backend would
// become N launches. And publishing info/err BEFORE done is closed makes the
// channel close the happens-before edge, so waiters read them without the lock.
// Refs: MGIT-122
type bootAttempt struct {
	done chan struct{}     // closed once info/err are final
	info model.SandboxInfo // the booted sandbox (valid when err is nil)
	err  error             // the boot's failure, shared with every waiter
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

// SetRegistry wires the durable sandbox registry. Set once at daemon wiring
// time, before the service handles any request. It is kept off the constructor
// like the other collaborators (parameter-count limit), but unlike them it is
// not optional for a SERVING daemon: without it, registrations live only in
// this process and vanish with it (MGIT-102). The daemon wiring always sets
// it, and the e2e gate proves a registration survives a daemon restart.
// Refs: FR-17.9, FR-17.10, MGIT-102
func (s *SandboxService) SetRegistry(r SandboxRegistry) {
	s.registry = r
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
	// Resolve the effective resource caps HERE, at the caller's boundary:
	// a request over the per-sandbox maximum is refused now, naming the
	// limit, rather than clamped (R-H212) or discovered at first exec; and
	// the resolved caps are recorded on the SandboxInfo so an agent can see
	// its ceiling before a build dies against it. Refs: R-H212, NFR-17.5
	policy, err := s.policy.Load(ctx)
	if err != nil {
		return nil, fmt.Errorf("sandbox register: load policy: %w", err)
	}
	if err := policy.EnforceResourceLimits(opts); err != nil {
		return nil, fmt.Errorf("sandbox register: %w", err)
	}
	applyPolicyDefaults(&opts, policy)

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

	if err := s.events.AppendSandboxEvent(ctx, createdEvent(id, opts, digest)); err != nil {
		return nil, fmt.Errorf("sandbox register: audit: %w", err)
	}

	now := s.clock().UTC()
	info := model.SandboxInfo{
		ID: id, TaskID: opts.TaskID, WorktreePath: opts.WorktreePath,
		ImageDigest: digest, NetworkMode: opts.Network.Mode,
		NetworkAllowlist: opts.Network.Allowlist,
		PublishPorts:     opts.PublishPorts,
		State:            model.StateCreated, CreatedAt: now,
		// The effective caps, resolved above — reported from registration on,
		// not only once a VM exists. Refs: R-H212
		CPUs: opts.CPUs, MemoryMB: opts.MemoryMB, DiskQuotaMB: opts.DiskQuotaMB,
	}
	// Make the registration DURABLE before it is considered registered. A
	// registration only this process knows about is one idle exit away from
	// never having happened, which is the whole of MGIT-102; so a registry
	// write failure fails the registration closed and records the terminal
	// event, rather than returning success for containment that will not be
	// there. Refs: FR-17.9, FR-17.10, FR-17.18, MGIT-102
	if err := s.persistRegistration(ctx, info, opts); err != nil {
		return nil, err
	}
	// lastActivity seeds the idle-suspend deadline from registration time;
	// expiresAt is resolved at boot (with the effective TTL) or lazily in the
	// reap sweep for a never-booted sandbox. Refs: NFR-17.3, FR-17.9
	s.byTask[opts.TaskID] = &sandboxReg{info: info, opts: opts, lastActivity: now}
	return &info, nil
}

// persistRegistration writes the durable registry row for a newly registered
// sandbox. On failure it appends the terminal `destroyed` event before
// returning: the `created` event is already in the append-only trail, and a
// trail ending at `created` for a sandbox that does not exist is a record
// asserting a sandbox that is not there — the second defect of MGIT-102, which
// must not be reintroduced at the very moment registration fails.
// Refs: FR-17.18, MGIT-102
func (s *SandboxService) persistRegistration(ctx context.Context, info model.SandboxInfo, opts model.SandboxLaunchOptions) error {
	if s.registry == nil {
		return nil
	}
	reg := &model.SandboxRegistration{
		Info: info, ImageRef: opts.ImageRef, TTL: opts.TTL, ConfineAgent: opts.ConfineAgent,
	}
	err := s.registry.UpsertSandbox(ctx, reg)
	if err == nil {
		return nil
	}
	auditErr := s.events.AppendSandboxEvent(ctx, &model.SandboxEvent{
		SandboxID: info.ID, TaskID: info.TaskID, EventType: model.EventDestroyed,
		Detail: `{"reason":"registration could not be made durable"}`,
	})
	return errors.Join(fmt.Errorf("sandbox register: persist registration: %w", err), auditErr)
}

// setPersistedState records a state transition in the durable registry so the
// NEXT daemon reconciles against what was last observed rather than against
// the registration-time state. A nil registry (memory-only wiring) is a no-op.
// Refs: FR-17.18, MGIT-102
func (s *SandboxService) setPersistedState(ctx context.Context, sandboxID, state string) error {
	if s.registry == nil {
		return nil
	}
	if err := s.registry.SetSandboxState(ctx, sandboxID, state); err != nil {
		return fmt.Errorf("sandbox registry: record %s state: %w", state, err)
	}
	return nil
}

// dropPersisted removes a torn-down sandbox from the durable registry so it is
// not rehydrated by the next daemon. The terminal event stays in
// sandbox_events — the history is not what is being deleted here.
// Refs: FR-17.9, MGIT-102
func (s *SandboxService) dropPersisted(ctx context.Context, sandboxID string) error {
	if s.registry == nil {
		return nil
	}
	if err := s.registry.DeleteSandbox(ctx, sandboxID); err != nil {
		return fmt.Errorf("sandbox registry: drop %s: %w", sandboxID, err)
	}
	return nil
}

// createdEvent builds the registration audit record. The created event must
// succeed before a sandbox is considered registered (no unaudited sandbox).
// Open network mode carries a recorded risk note (T3/T9 disabled) so the
// user-accepted risk is permanently attributable. Refs: FR-17.7, FR-17.18
func createdEvent(id string, opts model.SandboxLaunchOptions, digest string) *model.SandboxEvent {
	created := &model.SandboxEvent{
		SandboxID: id, TaskID: opts.TaskID, EventType: model.EventCreated,
		ImageDigest: digest, NetworkMode: opts.Network.Mode,
	}
	if note, risky := model.NetworkRiskNote(opts.Network.Mode); risky {
		created.Detail = fmt.Sprintf(`{"network_risk":%q}`, note)
	}
	return created
}

// EnsureRunning boots the task's sandbox if it is not already running
// (the first exec triggers this) and returns the running info. Policy
// defaults fill any unset resource limits. A boot failure leaves the
// registration intact (not booted) so the next attempt can retry.
//
// THE LOCK IS NOT HELD ACROSS THE BOOT. It is taken to decide what this call
// is — already running, joining a boot someone else started, or starting one —
// and taken again to record the outcome. In between, the VM boots with the
// mutex free, so a sibling sandbox's exec is answered in milliseconds instead
// of queueing behind a minute of boot and expiring on the client's socket
// deadline (MGIT-122).
//
// Three windows that narrowing could have opened are closed rather than
// tolerated: two callers for the SAME task converge on one bootAttempt (never
// two VMs); a teardown arriving mid-boot waits for it (awaitBootLocked) instead
// of dropping a registration whose VM is about to exist; and a policy staged
// during the boot is refused, because the launch has already taken its options
// (pendingRegLocked). Refs: FR-17.9, FR-17.10, MGIT-122
func (s *SandboxService) EnsureRunning(ctx context.Context, taskID string) (*model.SandboxInfo, error) {
	policy, err := s.policy.Load(ctx)
	if err != nil {
		return nil, fmt.Errorf("sandbox ensure-running: load policy: %w", err)
	}

	s.mu.Lock()
	reg, ok := s.byTask[taskID]
	if !ok {
		s.mu.Unlock()
		return nil, fmt.Errorf("%w: task %q", model.ErrSandboxNotFound, taskID)
	}
	if reg.booted {
		info := reg.info
		s.mu.Unlock()
		return &info, nil
	}
	if inFlight := reg.boot; inFlight != nil {
		s.mu.Unlock()
		return joinBoot(ctx, inFlight)
	}
	// This call owns the boot. Resolve the effective options under the lock and
	// take a COPY: nothing may read reg.opts through a boot it is not holding
	// the lock for, and the copy is what the VM is actually launched with.
	applyPolicyDefaults(&reg.opts, policy)
	opts := reg.opts
	attempt := &bootAttempt{done: make(chan struct{})}
	reg.boot = attempt
	s.mu.Unlock()

	launched, bootErr := s.runBoot(ctx, opts)
	return s.settleBoot(ctx, reg, attempt, launched, bootErr)
}

// joinBoot waits for a boot another caller started and returns ITS outcome.
//
// The waiter does not retry a failed boot: the winner already attempted it, and
// N callers each re-launching against a backend that has just refused is a boot
// storm, not a recovery. A canceled caller leaves the boot running — it belongs
// to the registration, not to whoever happened to trigger it. Refs: MGIT-122
func joinBoot(ctx context.Context, attempt *bootAttempt) (*model.SandboxInfo, error) {
	select {
	case <-attempt.done:
	case <-ctx.Done():
		return nil, fmt.Errorf("sandbox ensure-running: waiting for an in-flight boot: %w", ctx.Err())
	}
	if attempt.err != nil {
		return nil, attempt.err
	}
	info := attempt.info
	return &info, nil
}

// runBoot launches the VM and brings up its host-side controls, holding NO
// lock — this is the long part (a full boot) that used to serialize the daemon.
// Every failure fails closed, rolling back whatever it had already brought up,
// so the registration is left un-booted and retryable.
// Refs: FR-17.7, FR-17.8, FR-17.12, SEC-04, SEC-05, SEC-09, MGIT-122
func (s *SandboxService) runBoot(ctx context.Context, opts model.SandboxLaunchOptions) (*model.SandboxInfo, error) {
	launched, err := s.manager.Launch(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("sandbox ensure-running: %w", err)
	}
	// Start host egress enforcement (the allowlist proxy + DNS) before the
	// sandbox is considered up, so an allowlist guest never runs without its
	// host-side controls. none/open are no-ops in the controller.
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
		// so this is a no-op.
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
	// bind failure fails the boot closed so no half-published sandbox runs.
	if s.ports != nil && len(opts.PublishPorts) > 0 {
		if pubErr := s.ports.StartPublish(ctx, *launched, opts.PublishPorts); pubErr != nil {
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
	return launched, nil
}

// settleBoot re-takes the lock, records the boot's outcome on the registration
// and publishes it to every caller waiting on the attempt.
//
// reg is still the map's entry for this task, and is guaranteed to be: the only
// paths that remove a registration (Remove, Land, the TTL sweep) refuse to act
// on one with a boot in flight, so no teardown can have run in the window this
// function closes. Refs: MGIT-122, FR-17.9
func (s *SandboxService) settleBoot(ctx context.Context, reg *sandboxReg, attempt *bootAttempt,
	launched *model.SandboxInfo, bootErr error) (*model.SandboxInfo, error) {
	s.mu.Lock()
	// info/err are final BEFORE done is closed, and the close is what publishes
	// them: waiters read them off this happens-before edge without the lock.
	defer func() {
		reg.boot = nil
		close(attempt.done)
		s.mu.Unlock()
	}()
	if bootErr != nil {
		attempt.err = bootErr
		return nil, bootErr
	}
	if err := s.recordBootLocked(ctx, reg, launched); err != nil {
		attempt.err = err
		return nil, err
	}
	attempt.info = reg.info
	info := reg.info
	return &info, nil
}

// recordBootLocked audits the boot, makes it durable, and marks the
// registration running. Either bookkeeping failure rolls the VM back: a booted
// VM that the audit trail or the durable registry does not describe must never
// keep running (FR-17.18, MGIT-102). Caller holds the lock.
// Refs: FR-17.9, FR-17.18, NFR-17.3, R-H212, SEC-09, MGIT-102
func (s *SandboxService) recordBootLocked(ctx context.Context, reg *sandboxReg, launched *model.SandboxInfo) error {
	if auditErr := s.events.AppendSandboxEvent(ctx, &model.SandboxEvent{
		SandboxID: launched.ID, TaskID: reg.info.TaskID, EventType: model.EventResumed,
	}); auditErr != nil {
		return errors.Join(
			fmt.Errorf("sandbox ensure-running: audit: %w", auditErr),
			s.rollbackBoot(ctx, launched.ID))
	}
	if stateErr := s.setPersistedState(ctx, launched.ID, model.StateRunning); stateErr != nil {
		return errors.Join(stateErr, s.rollbackBoot(ctx, launched.ID))
	}
	reg.info = *launched
	// The backend's SandboxInfo does not carry the published-port mappings
	// (they are a service-level concern); restore them so List/Status keep
	// reporting them after boot. Likewise the effective resource caps: not
	// every backend echoes them back, and a sandbox that stopped reporting its
	// ceiling the moment it booted would hide it at exactly the point a build
	// runs into it. Refs: SEC-09, R-H212
	reg.info.PublishPorts = reg.opts.PublishPorts
	reg.info.CPUs, reg.info.MemoryMB, reg.info.DiskQuotaMB = reg.opts.CPUs, reg.opts.MemoryMB, reg.opts.DiskQuotaMB
	reg.booted = true
	// Record activity and the TTL deadline from the service clock (not the
	// backend's), so idle-suspend and TTL reap are deterministic.
	now := s.clock().UTC()
	reg.lastActivity = now
	if reg.opts.TTL > 0 {
		reg.expiresAt = now.Add(reg.opts.TTL)
		reg.info.ExpiresAt = reg.expiresAt
	}
	return nil
}

// awaitBootLocked returns taskID's registration with NO boot in flight,
// dropping and re-taking the lock while it waits. Caller holds the lock on
// entry and holds it on return, on every path including cancellation.
//
// This is what keeps the narrower boundary from becoming a lost VM. A teardown
// that ran mid-boot would delete the registration and append its terminal event
// while a VM was still being created for it: the VM would survive with nothing
// tracking it (the MGIT-103 shape), and the audit trail would record `resumed`
// after `destroyed` — a life that continues after it ended, which is exactly
// what the fleet soak's invariant I5 refuses. Waiting is bounded by the boot,
// and is per-task: a teardown of one sandbox never waits on another's boot.
// Refs: MGIT-122, MGIT-103, FR-17.18
func (s *SandboxService) awaitBootLocked(ctx context.Context, taskID string) (*sandboxReg, error) {
	for {
		reg, ok := s.byTask[taskID]
		if !ok {
			return nil, fmt.Errorf("%w: task %q", model.ErrSandboxNotFound, taskID)
		}
		inFlight := reg.boot
		if inFlight == nil {
			return reg, nil
		}
		s.mu.Unlock()
		select {
		case <-inFlight.done:
			s.mu.Lock()
		case <-ctx.Done():
			s.mu.Lock()
			return nil, fmt.Errorf("sandbox: waiting for task %q's boot to settle: %w", taskID, ctx.Err())
		}
	}
}

// rollbackBoot undoes a boot whose bookkeeping failed: it closes the published
// ports and host egress, then stops and removes the VM. It exists so every
// post-boot failure path fails closed the same way — a VM that the audit trail
// or the durable registry does not describe must never keep running. The
// registration stays un-booted and retryable. Refs: FR-17.18, FR-17.19, MGIT-102
func (s *SandboxService) rollbackBoot(ctx context.Context, sandboxID string) error {
	if s.ports != nil {
		s.ports.StopPublish(sandboxID)
	}
	if s.egress != nil {
		s.egress.StopEgress(sandboxID)
	}
	return errors.Join(
		s.manager.Stop(ctx, sandboxID, true),
		s.manager.Remove(ctx, sandboxID, true),
	)
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
// failure leaves the sandbox registered and retryable.
//
// A remove that arrives while THIS task's VM is booting waits for the boot to
// settle and then tears down what it produced, rather than racing it and
// leaving the VM behind (awaitBootLocked). Refs: FR-17.9, FR-17.18, MGIT-122
func (s *SandboxService) Remove(ctx context.Context, taskID string, force bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	reg, err := s.awaitBootLocked(ctx, taskID)
	if err != nil {
		return err
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
