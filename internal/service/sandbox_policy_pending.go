package service

import (
	"context"
	"fmt"

	"github.com/hyper-swe/mgit/internal/model"
)

// StagePendingEgressPolicy replaces the egress allowlist a REGISTERED-but-
// unbooted sandbox will boot with, so its microVM comes up already enforcing it.
//
// WHY STAGE RATHER THAN BOOT-THEN-APPLY. Provisioning is lazy by design
// (FR-17.9, FR-17.10): `mgit work --sandbox` registers a sandbox and the VM
// boots on first use. The policy verbs used to dial the VM's control channel
// regardless, so on the documented setup path — which is exactly where a user
// walk reaches the egress-policy step — they failed against a VM that was
// correctly absent (MGIT-109). Booting one merely to reconfigure it would work,
// but staging is strictly safer: the VM never runs under the policy the caller
// was replacing, not even for the instant between boot and mutation.
//
// IT IS DURABLE BEFORE IT IS REPORTED. The registry row is written first,
// because a stage held only in this process is one daemon restart away from a
// VM booting under the WIDER launch-time allowlist the caller had replaced —
// a silent fail-open at the moment containment is being relied on. A registry
// failure leaves the in-memory options untouched and returns an error.
//
// IT LOSES THE BOOT RACE LOUDLY. booted is re-checked under the same lock that
// guards Launch, so a boot landing between the caller reading the recorded state
// and this call returns ErrSandboxBooted having staged NOTHING — the caller
// re-routes to the live enforcer, and no retry can stage twice.
// Refs: MGIT-109, FR-17.9, FR-17.10, MGIT-72, MGIT-102, SEC-04
func (s *SandboxService) StagePendingEgressPolicy(
	ctx context.Context, sandboxID string, entries []string,
) (model.EgressPolicyState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	reg, err := s.pendingRegLocked(sandboxID)
	if err != nil {
		return model.EgressPolicyState{}, err
	}
	staged := append([]string(nil), entries...)
	if err := s.persistStagedLocked(ctx, reg, staged); err != nil {
		return model.EgressPolicyState{}, err
	}
	reg.opts.Network.Allowlist = staged
	// The reported launch policy must track what the VM will actually boot
	// with. Leaving it at the original list would make `sandbox status` and
	// `sandbox policy show` disagree about the same sandbox.
	reg.info.NetworkAllowlist = staged
	return pendingState(staged), nil
}

// PendingEgressPolicy reports the allowlist a registered-but-unbooted sandbox
// will boot with, flagged pending.
//
// It reports the entries rather than an empty policy for the same reason an
// unreachable enforcer is an error and not an empty policy (MGIT-72): an empty
// list reads as "nothing is permitted", and here the truth is "this is what will
// be permitted, once something starts enforcing". Refs: MGIT-109, MGIT-72, SEC-04
func (s *SandboxService) PendingEgressPolicy(
	_ context.Context, sandboxID string,
) (model.EgressPolicyState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	reg, err := s.pendingRegLocked(sandboxID)
	if err != nil {
		return model.EgressPolicyState{}, err
	}
	return pendingState(reg.opts.Network.Allowlist), nil
}

// pendingRegLocked resolves a sandbox ID to a registration whose VM has NOT
// booted, refusing anything else.
//
// It is keyed by the host-owned sandbox ID rather than the task, so a task that
// was re-bound between the caller's resolution and this call cannot redirect a
// policy onto a different sandbox (SEC-05). Caller holds the lock.
// Refs: MGIT-109, SEC-05
func (s *SandboxService) pendingRegLocked(sandboxID string) (*sandboxReg, error) {
	for _, reg := range s.byTask {
		if reg.info.ID != sandboxID {
			continue
		}
		if reg.booted {
			return nil, fmt.Errorf("%w: sandbox %s", model.ErrSandboxBooted, sandboxID)
		}
		return reg, nil
	}
	return nil, fmt.Errorf("%w: sandbox %q", model.ErrSandboxNotFound, sandboxID)
}

// persistStagedLocked writes the staged allowlist to the durable registry
// BEFORE the in-memory options are changed, so a failure leaves the two
// consistent and the caller correctly told nothing was staged. A nil registry
// (memory-only wiring) is a no-op. Caller holds the lock.
// Refs: MGIT-109, MGIT-102, FR-17.10
func (s *SandboxService) persistStagedLocked(
	ctx context.Context, reg *sandboxReg, staged []string,
) error {
	if s.registry == nil {
		return nil
	}
	info := reg.info
	info.NetworkAllowlist = staged
	row := &model.SandboxRegistration{
		Info: info, ImageRef: reg.opts.ImageRef,
		TTL: reg.opts.TTL, ConfineAgent: reg.opts.ConfineAgent,
	}
	if err := s.registry.UpsertSandbox(ctx, row); err != nil {
		return fmt.Errorf(
			"sandbox policy: stage pending allowlist for %s: %w "+
				"(nothing was staged; the sandbox will still boot with its current policy)",
			reg.info.ID, err)
	}
	return nil
}

// pendingState renders a staged allowlist. RuleCount stays 0 deliberately:
// nothing has compiled these entries yet, and a non-zero count would imply an
// enforcer had accepted them. Refs: MGIT-109
func pendingState(entries []string) model.EgressPolicyState {
	return model.EgressPolicyState{Entries: nonNil(entries), Pending: true}
}
