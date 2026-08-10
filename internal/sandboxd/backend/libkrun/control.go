package libkrun

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/backend/microvm"
	"github.com/hyper-swe/mgit/internal/sandboxd/egress"
	"github.com/hyper-swe/mgit/internal/sandboxd/vmctl"
)

// controlSocketPath is the per-VM host↔child control socket, under the sandbox
// state dir like every other per-VM socket — so the manager's single RemoveAll
// reclaims it at teardown (SEC-10, FR-17.19) and the path itself stays inside
// the 104-byte sun_path budget.
//
// It is NOT inside the staged worktree, which is the only part of the state
// dir the guest ever sees. That placement is what keeps the channel
// host-only: the guest has no filesystem route to it. Refs: MGIT-74, SEC-05
func controlSocketPath(stateDir string) string {
	return filepath.Join(stateDir, vmctl.SocketName)
}

// serveControlChannel binds and serves the child's control channel, returning
// a stop func (nil when there is nothing to serve).
//
// Only an allowlist-mode sandbox has a mutable policy, so only it gets a
// channel; none and open have no supervisor and nothing to mutate. That is
// reported by the ABSENCE of the socket, which the host side turns into an
// actionable error rather than a silent no-op. Refs: MGIT-74, MGIT-72
func serveControlChannel(spec vmSpec, sup *egress.Supervisor, logger *slog.Logger) (func(), error) {
	if sup == nil {
		return nil, nil
	}
	path := controlSocketPath(spec.StateDir)
	if err := checkSocketPathLen("vm control channel", path); err != nil {
		return nil, err
	}
	// A stale socket from a crashed predecessor would make Listen fail; the
	// state dir is per-VM, so removing it here is safe.
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("%w: clear stale vm control socket %s: %w",
			model.ErrSandboxBackendUnavailable, path, err)
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "unix", path)
	if err != nil {
		return nil, fmt.Errorf("%w: bind vm control channel %s: %w",
			model.ErrSandboxBackendUnavailable, path, err)
	}
	// Host-only by permission as well as by placement.
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("%w: restrict vm control channel %s: %w",
			model.ErrSandboxBackendUnavailable, path, err)
	}

	handler := &policyHandler{sup: sup, sandboxID: spec.SandboxID, taskID: spec.TaskID, logger: logger}
	go func() { _ = vmctl.Serve(ln, handler) }()
	logger.Info("vm control channel serving", "event", "vm_control_serving",
		"sandbox_id", spec.SandboxID, "socket", path)
	return func() { _ = ln.Close() }, nil
}

// policyHandler applies live policy mutations inside the child, where the
// authorizer actually lives.
type policyHandler struct {
	sup       *egress.Supervisor
	sandboxID string
	taskID    string
	logger    *slog.Logger
}

// SetPolicy replaces the running allowlist and, unless draining, kills
// established flows.
//
// ESTABLISHED FLOWS ARE KILLED by default (ADR-012): a caller who revokes
// registry egress and then runs untrusted code expects the grant gone, and a
// draining connection is precisely the exfiltration channel they revoked —
// against a hostile guest holding one open, "drain" can mean "never".
//
// The swap is atomic (SetRules compiles before taking the lock), so a flow is
// authorized against the old ruleset or the new one, never a mixture; a policy
// that does not compile leaves the running one in force and is reported as a
// failure. Refs: MGIT-74, MGIT-72, SEC-04
func (h *policyHandler) SetPolicy(entries []string, drain bool) (vmctl.Response, error) {
	if err := h.sup.Allowlist().SetRules(entries); err != nil {
		return vmctl.Response{}, err
	}
	al := h.sup.Allowlist()
	// Entries is read back out of the allowlist rather than echoed from the
	// request, so the reply states what is IN FORCE, not what was asked for.
	resp := vmctl.Response{Rules: al.Entries(), Entries: al.Rules(), Drained: drain}
	if !drain {
		resp.Killed = h.sup.Flows().CloseAll()
	}
	// Audited in the child's own structured log, which the parent captures as
	// the per-VM console log: a runtime-mutable policy with no record of when
	// it changed is worse than one that cannot change. Refs: FR-17.18
	h.logger.Info("vm egress policy changed",
		"event", "egress_policy_set", "sandbox_id", h.sandboxID, "task_id", h.taskID,
		"entries", entries, "rule_count", resp.Rules,
		"established_flows_killed", resp.Killed, "drained", drain)
	return resp, nil
}

// GetPolicy reports the allowlist this child is enforcing right now.
//
// It reads the LIVE allowlist, so after a mutation it disagrees with the
// sandbox's launch-time policy — which is the point: reporting the launch
// policy would tell a caller egress is open when it has been revoked.
// Refs: MGIT-72
func (h *policyHandler) GetPolicy() (vmctl.Response, error) {
	al := h.sup.Allowlist()
	return vmctl.Response{Rules: al.Entries(), Entries: al.Rules()}, nil
}

// NewPolicyClient returns the HOST side of the control channel for one
// sandbox, derived from the same (workDir, sandboxID) the manager uses — so
// the daemon needs nothing reported back to it to find the channel.
//
// It fails closed by construction: an absent socket (a sandbox that is not
// running, has no mutable policy, or predates this capability) surfaces as an
// error from the call, never as a false success. Refs: MGIT-74, MGIT-72
func NewPolicyClient(workDir, sandboxID string) vmctl.Client {
	return vmctl.Client{SocketPath: controlSocketPath(microvm.SandboxStateDir(workDir, sandboxID))}
}
