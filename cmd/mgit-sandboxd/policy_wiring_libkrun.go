//go:build cgo && !vzf && (darwin || (linux && libkrun))

package main

import (
	"context"
	"log/slog"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/backend/libkrun"
	"github.com/hyper-swe/mgit/internal/service"
)

// platformPolicyController is the libkrun backend's LIVE egress-policy
// enforcer, reached over the host→child control channel.
//
// It exists because libkrun's krun_start_enter never returns and consumes the
// calling process, so every VM runs in a re-exec'd child (ADR-010) — and the
// netstack gateway with its authorizer lives in that child. The daemon, which
// owns the CLI/MCP surface and the task bindings, therefore has no in-process
// route to the thing actually enforcing policy. The control channel is that
// route, and it is host-initiated only: the socket sits in the sandbox STATE
// dir, which the guest never mounts (SEC-05). Refs: MGIT-72, MGIT-74, ADR-010
func platformPolicyController(workDir string, logger *slog.Logger) service.EgressPolicyController {
	logger.Info("live egress policy wired", "event", "policy_wired", "backend", "libkrun")
	return libkrunPolicyController{workDir: workDir}
}

// libkrunPolicyController mutates and reads a running VM child's allowlist.
type libkrunPolicyController struct{ workDir string }

// SetEgressPolicy replaces the running allowlist inside the VM child, killing
// established flows unless drain is set.
//
// It FAILS CLOSED: an unreachable child (a sandbox that is not running, has no
// mutable policy, or predates this capability) surfaces as an error and never
// as a false success — a revoke that claims to have worked while the VM keeps
// enforcing the old policy leaves the caller running untrusted code believing
// egress is closed. Refs: MGIT-72, ADR-012, SEC-04
func (c libkrunPolicyController) SetEgressPolicy(
	_ context.Context, sandboxID string, entries []string, drain bool,
) (model.EgressPolicyChange, error) {
	resp, err := libkrun.NewPolicyClient(c.workDir, sandboxID).SetPolicy(entries, drain)
	if err != nil {
		return model.EgressPolicyChange{}, err
	}
	return model.EgressPolicyChange{
		Entries: resp.Entries, RuleCount: resp.Rules,
		Killed: resp.Killed, Drained: resp.Drained,
	}, nil
}

// EgressPolicy reports the allowlist the VM child is enforcing right now.
// Refs: MGIT-72
func (c libkrunPolicyController) EgressPolicy(
	_ context.Context, sandboxID string,
) (model.EgressPolicyState, error) {
	resp, err := libkrun.NewPolicyClient(c.workDir, sandboxID).GetPolicy()
	if err != nil {
		return model.EgressPolicyState{}, err
	}
	return model.EgressPolicyState{Entries: resp.Entries, RuleCount: resp.Rules}, nil
}
