package firecracker

import (
	"context"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/egress"
)

// EgressController adapts the host egress runner to the sandbox service's
// boot/teardown seam on this backend: at boot it binds the proxy and the
// restricted DNS on the sandbox's tap gateway under the policy the sandbox
// is booting with, and at teardown it tears them down.
//
// It lives here, beside GatewayFor, rather than in the daemon's wiring, so
// the live gate for a policy staged onto an unbooted sandbox (MGIT-156)
// runs the same adapter the daemon runs: the policy it reads is the one on
// the SandboxInfo the service hands it, which is what a staged allowlist
// replaces before boot (MGIT-109). Refs: FR-17.7, FR-17.8, SEC-04, MGIT-109, MGIT-156
type EgressController struct{ runner *egress.Runner }

// NewEgressController wraps a runner. A nil runner is the caller's wiring
// error and is refused at the first call, never masked.
func NewEgressController(runner *egress.Runner) EgressController {
	return EgressController{runner: runner}
}

// StartEgress brings up the sandbox's egress stack on its tap gateway under
// the network policy recorded on info. Refs: FR-17.8, SEC-04
func (c EgressController) StartEgress(ctx context.Context, info model.SandboxInfo) error {
	_, err := c.runner.Start(ctx, egress.Binding{
		SandboxID: info.ID,
		TaskID:    info.TaskID,
		GatewayIP: GatewayFor(info.ID),
		Policy:    model.NetworkPolicy{Mode: info.NetworkMode, Allowlist: info.NetworkAllowlist},
	})
	return err
}

// StopEgress tears the sandbox's egress stack down (idempotent).
func (c EgressController) StopEgress(sandboxID string) { _ = c.runner.Stop(sandboxID) }

// PolicyController adapts the same runner to the service's live-policy seam.
// On this backend the enforcer lives in the daemon's own process, so a
// mutation is a direct call — unlike libkrun, where it has to cross into a
// re-exec'd VM child. Refs: MGIT-72
type PolicyController struct{ runner *egress.Runner }

// NewPolicyController wraps a runner as the live policy enforcer.
func NewPolicyController(runner *egress.Runner) PolicyController {
	return PolicyController{runner: runner}
}

// SetEgressPolicy replaces the running allowlist, killing established flows
// unless drain is set. Refs: MGIT-72, ADR-012
func (c PolicyController) SetEgressPolicy(
	_ context.Context, sandboxID string, entries []string, drain bool,
) (model.EgressPolicyChange, error) {
	change, err := c.runner.SetPolicy(sandboxID, entries, drain)
	if err != nil {
		return model.EgressPolicyChange{}, err
	}
	return model.EgressPolicyChange{
		Entries: change.Entries, RuleCount: change.RuleCount,
		Killed: change.Killed, Drained: change.Drained,
	}, nil
}

// EgressPolicy reports the allowlist in force. Refs: MGIT-72
func (c PolicyController) EgressPolicy(
	_ context.Context, sandboxID string,
) (model.EgressPolicyState, error) {
	state, err := c.runner.Policy(sandboxID)
	if err != nil {
		return model.EgressPolicyState{}, err
	}
	return model.EgressPolicyState{Entries: state.Entries, RuleCount: state.RuleCount}, nil
}
