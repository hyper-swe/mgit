package mcp

import (
	"context"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/hyper-swe/mgit/internal/controlproto"
)

// SandboxPolicyClient is the sandbox daemon's live egress-policy surface.
// *sandboxd.Client satisfies it. Refs: MGIT-72
type SandboxPolicyClient interface {
	SetEgressPolicy(ctx context.Context, taskID string, entries []string, drain bool) (*controlproto.PolicyResult, error)
	EgressPolicy(ctx context.Context, taskID string) (*controlproto.PolicyResult, error)
}

// SandboxPolicyConnector resolves a live daemon for one tool call.
//
// It is a connector rather than a held client because the daemon is
// socket-activated and may come and go under a long-lived MCP server; dialing
// per call also means a dead daemon surfaces as an error on the call that
// needed it, not as a server that silently stopped working. Refs: MGIT-72
type SandboxPolicyConnector func(ctx context.Context) (SandboxPolicyClient, error)

// WithSandboxPolicy wires the live egress-policy tool to a sandbox daemon.
//
// Without it the tool is still REGISTERED — so an agent can discover it and be
// told plainly that no daemon is available — but every call fails closed. A
// registered tool that quietly reported success with nothing enforcing behind
// it would be the worst outcome this verb has. Refs: MGIT-72, SEC-04
func WithSandboxPolicy(connect SandboxPolicyConnector) Option {
	return func(c *config) { c.sandboxPolicy = connect }
}

// sandboxPolicyActions is the closed verb vocabulary. An unknown action is
// refused rather than defaulted: defaulting a policy verb would let a typo
// pick a different security posture than the caller intended.
var sandboxPolicyActions = map[string]bool{"set": true, "revoke": true, "show": true}

// sandboxPolicyTool changes or reports the egress policy of a RUNNING sandbox
// without relaunching it.
//
// AN AGENT IS THE INTENDED CALLER — this is the surface HyperSwe's
// provisioning uses: grant package-registry egress for setup, then revoke it
// before the untrusted dev/test run. Every argument is untrusted and validated
// here before anything reaches the daemon. Refs: MGIT-72, FR-17.8, SEC-04, SEC-05
func (s *Server) sandboxPolicyTool(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	action, _ := args["action"].(string)
	taskID, _ := args["task_id"].(string)
	drain, _ := args["drain"].(bool)

	if action == "" {
		return mcp.NewToolResultError("action is required (one of: set, revoke, show)"), nil
	}
	if !sandboxPolicyActions[action] {
		return mcp.NewToolResultError(
			fmt.Sprintf("unknown action %q (expected one of: set, revoke, show)", action)), nil
	}
	if taskID == "" {
		return mcp.NewToolResultError("task_id is required"), nil
	}
	if err := validateTaskID(taskID); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	allow, err := stringList(args["allow"])
	if err != nil {
		// MCP convention: a bad argument is a tool RESULT with IsError set,
		// not a Go error, so the agent sees the reason instead of a transport
		// failure. Every handler in this server does the same.
		//nolint:nilerr // intentional: reported as a tool result, not a Go error
		return mcp.NewToolResultError("allow: " + err.Error()), nil
	}
	// The same guard as the CLI: `set` with no destinations and `revoke` are
	// the same operation underneath, which is exactly why an accidental empty
	// `set` must not silently revoke everything.
	if action == "set" && len(allow) == 0 {
		return mcp.NewToolResultError(
			"set requires at least one allow entry; use action \"revoke\" to remove all egress"), nil
	}

	if s.sandboxPolicy == nil {
		return mcp.NewToolResultError(
			"the mgit sandbox daemon is not wired into this MCP server, so no egress policy " +
				"can be changed or read; the policy was NOT changed"), nil
	}
	cl, err := s.sandboxPolicy(ctx)
	if err != nil {
		//nolint:nilerr // intentional: reported as a tool result, not a Go error
		return mcp.NewToolResultError(
			"the mgit sandbox daemon is unavailable: " + err.Error() +
				"; the policy was NOT changed"), nil
	}

	var res *controlproto.PolicyResult
	switch action {
	case "show":
		res, err = cl.EgressPolicy(ctx, taskID)
	case "revoke":
		// nil entries: the replacement policy permits nothing.
		res, err = cl.SetEgressPolicy(ctx, taskID, nil, drain)
	default: // set
		res, err = cl.SetEgressPolicy(ctx, taskID, allow, drain)
	}
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if res == nil {
		return mcp.NewToolResultError("the sandbox daemon returned no policy result"), nil
	}
	return jsonResult(res), nil
}

// stringList coerces an MCP array argument to []string, refusing a non-string
// element rather than dropping it — a silently skipped allowlist entry is a
// destination the caller believes is permitted and is not.
func stringList(v any) ([]string, error) {
	if v == nil {
		return nil, nil
	}
	raw, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("must be an array of strings")
	}
	out := make([]string, 0, len(raw))
	for i, e := range raw {
		s, ok := e.(string)
		if !ok {
			return nil, fmt.Errorf("entry %d is not a string", i)
		}
		if err := validateText("allow", s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}
