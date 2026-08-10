package service

import (
	"context"
	"encoding/json"
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
	events SandboxEventAppender
	clock  func() time.Time
}

// NewEgressPolicyService wires the service. Every dependency is required: a
// service missing its enforcer, its audit sink or its clock would fail open at
// exactly the moment a caller is relying on it. Refs: MGIT-72
func NewEgressPolicyService(
	ctrl EgressPolicyController, events SandboxEventAppender, clock func() time.Time,
) (*EgressPolicyService, error) {
	switch {
	case ctrl == nil:
		return nil, fmt.Errorf("egress policy service: controller must not be nil")
	case events == nil:
		return nil, fmt.Errorf("egress policy service: event appender must not be nil")
	case clock == nil:
		return nil, fmt.Errorf("egress policy service: clock must not be nil")
	}
	return &EgressPolicyService{ctrl: ctrl, events: events, clock: clock}, nil
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

	change, err := s.ctrl.SetEgressPolicy(ctx, info.ID, entries, drain)
	if err != nil {
		// Recorded as NOT applied. A trail that logged the request and then
		// stayed silent would read as a successful change, and a caller who
		// believes egress is closed when it is open is the whole hazard.
		_ = s.audit(ctx, info, policyDetail{
			Phase: policyPhaseFailed, Entries: nonNil(entries), Drain: drain, Error: err.Error(),
		})
		return nil, fmt.Errorf("egress policy: %w", err)
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

// Show reports the policy a running sandbox is enforcing right now.
//
// It is deliberately NOT audited: a read changes nothing, and a trail padded
// with reads is one nobody reviews. Refs: MGIT-72
func (s *EgressPolicyService) Show(
	ctx context.Context, info model.SandboxInfo,
) (*model.EgressPolicyState, error) {
	if err := requireMutablePolicy(info); err != nil {
		return nil, err
	}
	state, err := s.ctrl.EgressPolicy(ctx, info.ID)
	if err != nil {
		return nil, fmt.Errorf("egress policy: %w", err)
	}
	return &state, nil
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
const (
	policyPhaseRequested = "requested"
	policyPhaseApplied   = "applied"
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
	Error     string   `json:"error,omitempty"`
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
