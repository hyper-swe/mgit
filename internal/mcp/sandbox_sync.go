package mcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/hyper-swe/mgit/internal/model"
)

// SandboxSyncer is the sandbox-daemon capability this tool needs: re-stage a
// task's host worktree into its running guest, or classify without touching
// it. *sandboxd.Client satisfies it. Refs: MGIT-76
type SandboxSyncer interface {
	SyncWorktree(ctx context.Context, taskID string, opts model.WorktreeSyncOptions) (*model.WorktreeSyncReport, error)
}

// SandboxConnector resolves a live sandbox daemon. It is injected rather than
// dialed here so the tool stays testable without spawning a daemon, and so the
// MCP server keeps no knowledge of activation.
type SandboxConnector func(ctx context.Context) (SandboxSyncer, error)

// WithSandboxSync wires the sandbox daemon connector, enabling
// `mgit_sandbox_sync`.
//
// The tool is registered unconditionally so the documented surface does not
// depend on how the server was constructed; without a connector it reports the
// daemon unavailable, which is the truth, rather than fabricating a sync.
// Refs: MGIT-76
func WithSandboxSync(connect SandboxConnector) Option {
	return func(c *config) { c.sandboxConnect = connect }
}

// sandboxSyncTool re-stages a task's host worktree into its running sandbox,
// or reports the classification for a dry run.
//
// An agent is a first-class caller of this verb: it is the one sandbox
// operation an agent loop needs BETWEEN rounds, and its --dry-run form is the
// only way to learn which paths diverged without running a command in the
// guest and being refused. Every argument is validated at the boundary before
// it reaches the daemon (MGIT-41). Refs: MGIT-76, FR-17.40, FR-10.2, ADR-011
func (s *Server) sandboxSyncTool(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	taskID, _ := req.GetArguments()["task_id"].(string)
	force, _ := req.GetArguments()["force"].(bool)
	dryRun, _ := req.GetArguments()["dry_run"].(bool)
	if taskID == "" {
		return mcp.NewToolResultError("task_id is required"), nil
	}
	if err := validateTaskID(taskID); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if s.sandboxConnect == nil {
		return mcp.NewToolResultError("sandbox sync is unavailable: this mgit MCP server was started " +
			"without a sandbox daemon connector"), nil
	}
	client, err := s.sandboxConnect(ctx)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("sandbox daemon unavailable: %v", err)), nil
	}

	report, err := client.SyncWorktree(ctx, taskID,
		model.WorktreeSyncOptions{Force: force, DryRun: dryRun})
	if err != nil {
		// A refusal is an ERROR result — an agent must never read it as a
		// completed sync — and it carries the conflicting paths, because
		// "conflict" alone gives the agent nothing to act on.
		return mcp.NewToolResultError(syncErrorText(err, report)), nil
	}
	return jsonResult(report), nil
}

// syncErrorText renders a failed sync for an agent: the daemon's message, plus
// every conflicting path and reason when the failure was a conflict.
func syncErrorText(err error, report *model.WorktreeSyncReport) string {
	var b strings.Builder
	b.WriteString(err.Error())
	if report == nil || len(report.Conflicts) == 0 {
		return b.String()
	}
	for _, c := range report.Conflicts {
		fmt.Fprintf(&b, "\n  %s (%s)", c.Path, c.Reason)
	}
	b.WriteString("\n  land the guest's work, or re-run with force=true to overwrite it " +
		"(every overwritten path is reported)")
	return b.String()
}
