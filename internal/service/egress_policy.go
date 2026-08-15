package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/hyper-swe/mgit/internal/model"
)

// EgressPolicyController is the seam to whatever is actually enforcing egress
// for a RUNNING sandbox.
//
// It exists because the enforcer is not in the same place on every backend:
// on firecracker it is the daemon's own egress runner, while on libkrun it
// lives in a re-exec'd VM child reachable only over the host→child control
// channel (ADR-010). The service must behave identically against both, so it
// depends on this contract and never on either implementation.
//
// AUTHORITY. Every implementation is HOST-side. Nothing reachable from the
// guest may call it — the guest's channels are exec, land and the egress data
// path, none of which mutate policy (SEC-05). Refs: MGIT-72, SEC-04, SEC-05
type EgressPolicyController interface {
	// SetEgressPolicy replaces the running allowlist, killing established
	// flows unless drain is set, and reports what it did.
	SetEgressPolicy(ctx context.Context, sandboxID string, entries []string, drain bool) (model.EgressPolicyChange, error)
	// EgressPolicy reports the allowlist in force. It must fail on an
	// unknown sandbox rather than return an empty policy.
	EgressPolicy(ctx context.Context, sandboxID string) (model.EgressPolicyState, error)
}

// PendingPolicyStager is the seam to a sandbox that is REGISTERED but whose
// microVM has not booted — the state `mgit work --sandbox` leaves behind, and
// the state a user walk reaches the egress-policy step in (FR-17.9, FR-17.10).
//
// WHY IT EXISTS. The policy verbs used to dial the VM's control channel
// unconditionally, so on the documented setup path they failed against a VM
// that lazy provisioning had deliberately not booted (MGIT-109). The fix is not
// to boot one: a policy staged onto the pending launch means the VM never runs
// under the policy the caller was replacing, not even for the moment between
// boot and mutation. That is strictly safer than boot-then-apply.
//
// IT MUST LOSE THE RACE LOUDLY. Both methods return ErrSandboxBooted, with
// nothing staged, if the VM booted after the caller read the recorded state.
// A staged policy reported as an enforced one is exactly the lie the fail-closed
// contract exists to prevent. Refs: MGIT-109, FR-17.10, MGIT-72, SEC-04
type PendingPolicyStager interface {
	// StagePendingEgressPolicy replaces the allowlist the sandbox will boot
	// with, durably, and reports it as pending.
	StagePendingEgressPolicy(ctx context.Context, sandboxID string, entries []string) (model.EgressPolicyState, error)
	// PendingEgressPolicy reports the allowlist the sandbox will boot with.
	PendingEgressPolicy(ctx context.Context, sandboxID string) (model.EgressPolicyState, error)
}

// EgressPolicyService changes and reports the egress policy of a RUNNING
// sandbox without relaunching it.
//
// WHY IT EXISTS. Provisioning wants package-registry egress during setup and
// wants it GONE before the untrusted dev/test run. Until now the only revoke
// was relaunch, which destroys the environment that was just provisioned — so
// callers held egress open for the whole run and disclosed it, a weaker
// posture than they intended.
//
// It is tractable because the authorizer is consulted PER CONNECTION: once the
// ruleset is swapped, the next flow is decided against the new policy with no
// VM involvement. Established flows are the design question, and the answer is
// recorded in ADR-012 — killed by default, drained only on request.
// Refs: MGIT-72, FR-17.8, FR-17.18, SEC-04, SEC-05, ADR-012
type EgressPolicyService struct {
	ctrl   EgressPolicyController
	stager PendingPolicyStager
	events SandboxEventAppender
	clock  func() time.Time
}

// NewEgressPolicyService wires the service. Every dependency is required: a
// service missing its enforcer, its audit sink or its clock would fail open at
// exactly the moment a caller is relying on it — and one missing its pending
// stager would fall back to dialing a VM that lazy provisioning has not booted,
// which is the MGIT-109 defect itself. Refs: MGIT-72, MGIT-109
func NewEgressPolicyService(
	ctrl EgressPolicyController, stager PendingPolicyStager,
	events SandboxEventAppender, clock func() time.Time,
) (*EgressPolicyService, error) {
	switch {
	case ctrl == nil:
		return nil, fmt.Errorf("egress policy service: controller must not be nil")
	case stager == nil:
		return nil, fmt.Errorf("egress policy service: pending policy stager must not be nil")
	case events == nil:
		return nil, fmt.Errorf("egress policy service: event appender must not be nil")
	case clock == nil:
		return nil, fmt.Errorf("egress policy service: clock must not be nil")
	}
	return &EgressPolicyService{ctrl: ctrl, stager: stager, events: events, clock: clock}, nil
}

// Set replaces a running sandbox's egress allowlist. An EMPTY entry list is a
// full revoke — the same verb, so "set" and "revoke" cannot drift apart.
//
// ESTABLISHED FLOWS ARE KILLED unless drain is set (ADR-012): a caller who
// revokes registry egress and then runs untrusted code expects the grant gone,
// and a draining connection is precisely the exfiltration channel they just
// revoked — against a hostile guest holding one open, "drain" can mean "never".
//
// AUDIT ORDER IS FAIL-CLOSED AND RECORDS BOTH ENDS. The request is appended
// BEFORE the mutation, so no widening can take effect unaudited and an audit
// sink that cannot be written blocks the change entirely. The OUTCOME is
// appended after, because the reviewer's question is not only "what was asked"
// but "what was actually enforced, and what did it terminate" — and a trail
// that recorded only intentions would claim a policy changed when the enforcer
// refused. Refs: MGIT-72, FR-17.18, SEC-04, ADR-012
// IT HONORS LAZY PROVISIONING. A sandbox that is registered but not booted has
// no enforcer to mutate, so the policy is STAGED onto its pending launch and
// the VM comes up already enforcing it (MGIT-109). Booting a guest merely to
// reconfigure it would be both wasteful and weaker — the VM would run under the
// replaced policy for the moment between boot and mutation.
// Refs: MGIT-72, FR-17.9, FR-17.10, FR-17.18, SEC-04, ADR-012, MGIT-109
func (s *EgressPolicyService) Set(
	ctx context.Context, info model.SandboxInfo, entries []string, drain bool,
) (*model.EgressPolicyChange, error) {
	if err := requireMutablePolicy(info); err != nil {
		return nil, err
	}
	if err := s.audit(ctx, info, policyDetail{
		Phase: policyPhaseRequested, Entries: nonNil(entries), Drain: drain,
	}); err != nil {
		return nil, fmt.Errorf("egress policy: audit: %w", err)
	}
	if notYetBooted(info.State) {
		change, err := s.stageSet(ctx, info, entries, drain)
		if !errors.Is(err, model.ErrSandboxBooted) {
			return change, err
		}
		// Lost race: the VM booted between the recorded state and the stage,
		// and NOTHING was staged. Re-route to the live enforcer rather than
		// report a staged policy as an enforced one. The sandbox is running
		// now, so the diagnosis must describe a running one.
		info.State = model.StateRunning
	}
	return s.applyLive(ctx, info, entries, drain)
}

// stageSet stages the policy onto a not-yet-booted sandbox's pending launch.
//
// It returns ErrSandboxBooted UNWRAPPED-BY-errors.Is and with nothing staged
// when it loses the boot race, which is the signal Set re-routes on; every
// other failure is audited as not-applied and returned. Refs: MGIT-109, FR-17.18
func (s *EgressPolicyService) stageSet(
	ctx context.Context, info model.SandboxInfo, entries []string, drain bool,
) (*model.EgressPolicyChange, error) {
	state, err := s.stager.StagePendingEgressPolicy(ctx, info.ID, entries)
	if err != nil {
		if errors.Is(err, model.ErrSandboxBooted) {
			return nil, err
		}
		_ = s.audit(ctx, info, policyDetail{
			Phase: policyPhaseFailed, Entries: nonNil(entries), Drain: drain, Error: err.Error(),
		})
		return nil, stageFailure(info, err)
	}
	// Killed is 0 and Drained false by construction: a VM that has never run
	// has no established flows to terminate or drain.
	change := &model.EgressPolicyChange{
		Entries: state.Entries, RuleCount: state.RuleCount, Pending: true,
	}
	if auditErr := s.audit(ctx, info, policyDetail{
		Phase: policyPhaseStaged, Entries: nonNil(change.Entries), Drain: drain, Pending: true,
	}); auditErr != nil {
		// The policy IS staged; say so rather than implying it is not.
		return change, fmt.Errorf(
			"egress policy: the policy WAS staged but the audit record failed: %w", auditErr)
	}
	return change, nil
}

// applyLive mutates the enforcer of a sandbox whose VM is running.
// Refs: MGIT-72, FR-17.18, SEC-04
func (s *EgressPolicyService) applyLive(
	ctx context.Context, info model.SandboxInfo, entries []string, drain bool,
) (*model.EgressPolicyChange, error) {
	change, err := s.ctrl.SetEgressPolicy(ctx, info.ID, entries, drain)
	if err != nil {
		// Recorded as NOT applied. A trail that logged the request and then
		// stayed silent would read as a successful change, and a caller who
		// believes egress is closed when it is open is the whole hazard.
		_ = s.audit(ctx, info, policyDetail{
			Phase: policyPhaseFailed, Entries: nonNil(entries), Drain: drain, Error: err.Error(),
		})
		return nil, diagnose(info, err)
	}

	if err := s.audit(ctx, info, policyDetail{
		Phase: policyPhaseApplied, Entries: nonNil(change.Entries), Drain: drain,
		RuleCount: change.RuleCount, Killed: change.Killed, Drained: change.Drained,
	}); err != nil {
		// The policy IS changed; say so rather than implying it is not.
		return &change, fmt.Errorf(
			"egress policy: the policy WAS changed but the audit record failed: %w", err)
	}
	return &change, nil
}

// Show reports the policy a sandbox is enforcing right now — or, for one whose
// VM has not booted, the policy it WILL enforce, flagged pending.
//
// A pending policy is never presented as a live one. That distinction is the
// same one the fail-closed contract rests on: "these entries are being
// enforced" and "these entries will be enforced once something starts" are
// different facts, and a caller who confuses them runs untrusted code believing
// a line is being held that nothing is holding yet.
//
// It is deliberately NOT audited: a read changes nothing, and a trail padded
// with reads is one nobody reviews. Refs: MGIT-72, MGIT-109, FR-17.10, SEC-04
func (s *EgressPolicyService) Show(
	ctx context.Context, info model.SandboxInfo,
) (*model.EgressPolicyState, error) {
	if err := requireMutablePolicy(info); err != nil {
		return nil, err
	}
	if notYetBooted(info.State) {
		state, err := s.stager.PendingEgressPolicy(ctx, info.ID)
		switch {
		case err == nil:
			state.Pending = true
			return &state, nil
		case !errors.Is(err, model.ErrSandboxBooted):
			return nil, stageFailure(info, err)
		}
		// Lost race, as in Set: it is running now, so read the enforcer.
		info.State = model.StateRunning
	}
	state, err := s.ctrl.EgressPolicy(ctx, info.ID)
	if err != nil {
		return nil, diagnose(info, err)
	}
	return &state, nil
}

// notYetBooted reports whether the RECORDED state says no VM is running for
// this sandbox: registered but never booted (created), or booted and paused
// (suspended, whose VM was stopped and whose next use re-launches it from the
// same options). Both mean there is no live enforcer, and both are correctly
// served by staging onto the pending launch.
//
// Any other state — INCLUDING an unrecorded or unrecognized one — routes to the
// live enforcer, which fails closed. Defaulting the unknown case to staging
// would report a policy as pending for a sandbox that may be running under the
// old one. Refs: MGIT-109, FR-17.10, NFR-17.3, SEC-04
func notYetBooted(state string) bool {
	return state == model.StateCreated || state == model.StateSuspended
}

// diagnose turns an enforcer failure into one carrying a STABLE failure code
// and a message that names which condition actually applies.
//
// Three genuinely different failures used to share one string with a two-cause
// guess ("the sandbox may not be running, or its VM predates this capability").
// Each has a different remedy, and the shrug is why a live bug report was
// misattributed to two unrelated tickets. The daemon holds the recorded state
// and the enforcer reports host-side evidence; between them the condition is
// known. Refs: MGIT-109, MGIT-104, MGIT-99, MGIT-74, R-H233
func diagnose(info model.SandboxInfo, err error) error {
	var ch *model.EgressChannelUnreachableError
	if !errors.As(err, &ch) {
		// Not an unreachable enforcer: some other refusal (a policy that does
		// not compile, an unknown sandbox). It is still a policy-verb failure,
		// so it still carries a code — UNKNOWN, never the nearest of the three.
		return &model.EgressPolicyError{
			Code:   model.EgressFailureUnknown,
			Reason: "egress policy: the running policy was NOT changed",
			Cause:  err,
		}
	}
	code, reason := unreachableCondition(info, ch)
	return &model.EgressPolicyError{Code: code, Reason: reason, Cause: err}
}

// stageFailure classifies a failure on the PENDING-launch route.
//
// The sandbox condition here is NOT_BOOTED by construction — this route is only
// taken for a sandbox with no VM — so that is the code, and the prose names what
// actually went wrong. An integrator branching on NOT_BOOTED learns the true
// thing: nothing is enforcing yet, and the policy they asked for is not waiting
// either. A sandbox that could not be resolved at all is UNKNOWN instead: its VM
// condition is precisely what could not be established, and guessing would be
// the defect this ticket is about one layer down.
// Refs: MGIT-109, R-H233, FR-17.10, SEC-04
func stageFailure(info model.SandboxInfo, err error) error {
	code := model.EgressFailureNotBooted
	if errors.Is(err, model.ErrSandboxNotFound) {
		code = model.EgressFailureUnknown
	}
	return &model.EgressPolicyError{
		Code: code,
		Reason: fmt.Sprintf(
			"egress policy: sandbox %s for task %s has NOT booted yet, and the policy could not "+
				"be staged onto its pending launch; it will still boot with its current policy",
			info.ID, info.TaskID),
		Cause: err,
	}
}

// unreachableCondition names the one condition that applies, from the recorded
// sandbox state plus the host-side evidence the enforcer observed.
//
// The evidence split: a VM that bound the control channel leaves its socket
// behind when its process dies, so a socket that EXISTS but does not answer is
// a dead guest. A VM whose host-side state directory exists but which never
// created a socket at all was launched by a build predating the channel.
//
// A sandbox in neither a running nor a not-yet-booted state is UNKNOWN, not the
// nearest of the three: this build cannot reason about it, and saying "recorded
// as running" about a state it does not recognize would be a confident wrong
// answer. Refs: MGIT-109, MGIT-99, MGIT-74, R-H233
func unreachableCondition(info model.SandboxInfo, ch *model.EgressChannelUnreachableError) (code, reason string) {
	switch {
	case info.State != model.StateRunning:
		return model.EgressFailureUnknown, fmt.Sprintf(
			"egress policy: sandbox %s for task %s is in state %q, which this build cannot "+
				"reason about, and its enforcer at %s is unreachable. The running policy was "+
				"NOT changed",
			info.ID, info.TaskID, info.State, ch.SocketPath)
	case ch.VMStateSeen && !ch.ChannelSeen:
		return model.EgressFailureVersionPredates, fmt.Sprintf(
			"egress policy: sandbox %s for task %s is running, but its VM was launched WITHOUT a "+
				"control channel — it predates live egress policy. Its launch-time allowlist is "+
				"still enforced and cannot be changed in place; relaunch it with this build "+
				"(`mgit sandbox remove --task %s`, then re-run your task). "+
				"The running policy was NOT changed",
			info.ID, info.TaskID, info.TaskID)
	default:
		return model.EgressFailureBootedDied, fmt.Sprintf(
			"egress policy: sandbox %s for task %s is recorded as running, but its VM control "+
				"channel at %s is not answering: the guest has exited or was killed, so nothing "+
				"is enforcing egress for it now. Tear it down and relaunch "+
				"(`mgit sandbox remove --task %s`). The running policy was NOT changed",
			info.ID, info.TaskID, ch.SocketPath, info.TaskID)
	}
}

// requireMutablePolicy refuses a sandbox that has no allowlist to mutate.
//
// It fails closed rather than reporting a cheerful no-op: "revoke succeeded"
// for a sandbox that was never enforcing an allowlist is the most dangerous
// possible answer, because the caller then runs untrusted code believing
// egress was narrowed. none mode has no egress at all and open mode has no
// allowlist — both are told so by name. Refs: MGIT-72, SEC-04
func requireMutablePolicy(info model.SandboxInfo) error {
	switch info.NetworkMode {
	case model.NetworkModeAllowlist:
		return nil
	case model.NetworkModeNone:
		return fmt.Errorf(
			"egress policy: sandbox for task %s runs with network mode %q — it has no egress "+
				"at all, so there is no allowlist to change",
			info.TaskID, model.NetworkModeNone)
	default:
		return fmt.Errorf(
			"egress policy: sandbox for task %s runs with network mode %q — live policy applies "+
				"to allowlist mode only; relaunch with --network allowlist to enforce one",
			info.TaskID, info.NetworkMode)
	}
}

// Audit phases. One event type, three phases, so a reviewer can reconstruct
// both what was asked for and what actually happened.
// policyPhaseStaged is deliberately NOT "applied": a record claiming a running
// policy changed, for a sandbox whose VM has not booted, is the same lie the
// Pending field exists to prevent — and it is the record a reviewer would use
// to reconstruct what was enforced. Refs: MGIT-109, FR-17.18
const (
	policyPhaseRequested = "requested"
	policyPhaseApplied   = "applied"
	policyPhaseStaged    = "staged"
	policyPhaseFailed    = "failed"
)

// policyDetail is the JSON body of a policy_changed audit record. Every field
// is host-observed; nothing here is guest-sourced (SEC-05).
type policyDetail struct {
	Phase     string   `json:"phase"`
	Entries   []string `json:"entries"`
	RuleCount int      `json:"rule_count,omitempty"`
	Drain     bool     `json:"drain"`
	Drained   bool     `json:"drained,omitempty"`
	Killed    int      `json:"established_flows_killed"`
	// Pending marks a record whose policy was staged onto a pending launch and
	// is NOT yet being enforced. Refs: MGIT-109, FR-17.10
	Pending bool   `json:"pending,omitempty"`
	Error   string `json:"error,omitempty"`
}

// audit appends one append-only policy_changed record naming the sandbox, its
// task binding, and the change. Refs: FR-17.18
func (s *EgressPolicyService) audit(ctx context.Context, info model.SandboxInfo, d policyDetail) error {
	body, err := json.Marshal(d)
	if err != nil {
		return fmt.Errorf("encode policy detail: %w", err)
	}
	return s.events.AppendSandboxEvent(ctx, &model.SandboxEvent{
		SandboxID:   info.ID,
		TaskID:      info.TaskID,
		EventType:   model.EventPolicyChanged,
		NetworkMode: info.NetworkMode,
		Detail:      string(body),
		CreatedAt:   s.clock().UTC(),
	})
}

// nonNil renders an empty policy as [] rather than null in the audit record,
// so "everything was revoked" is legible instead of ambiguous.
func nonNil(entries []string) []string {
	if entries == nil {
		return []string{}
	}
	return entries
}
