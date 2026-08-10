package mcp

import (
	"context"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/hyper-swe/mgit/internal/model"
)

// SandboxExportClient is the control-plane capability the artifact-export tool
// needs: bring a host-named path out of a task's sandbox to a host-named
// destination. *sandboxd.Client satisfies it. The MCP server talks to the
// daemon ONLY through this — never a manager, never a store.
// Refs: MGIT-73, ADR-011
type SandboxExportClient interface {
	ExportArtifact(ctx context.Context, taskID string,
		req model.ArtifactExportRequest) (*model.ArtifactExportResult, error)
}

// SandboxExportConnector resolves a live sandbox daemon, or reports why it is
// unavailable. It is injected so the MCP server never spawns or locates a
// daemon itself (and so tests drive the tool without one).
type SandboxExportConnector func(ctx context.Context) (SandboxExportClient, error)

// WithSandboxExport wires the sandbox daemon connector that backs
// mgit_sandbox_export. Without it the tool is still registered — an agent must
// be able to discover the capability and read why it is unavailable — but every
// call fails closed with that reason rather than doing something weaker.
// Refs: MGIT-73
func WithSandboxExport(connect SandboxExportConnector) Option {
	return func(c *config) { c.sandboxExport = connect }
}

// registerSandboxExportTool adds the artifact-export tool. It is the FIRST
// sandbox verb on the MCP surface, and it is here because the caller is an
// agent: the agent is the one that just built the artifact and knows it is
// worth keeping. Refs: MGIT-73, MGIT-50
func (s *Server) registerSandboxExportTool() {
	s.mcpServer.AddTool(mcp.NewTool("mgit_sandbox_export",
		mcp.WithDescription("Export a guest-built artifact (node_modules, build cache) out of a task's "+
			"sandbox to a host path. Both paths are host-named; the destination must not already exist "+
			"(collisions are refused), escaping symlinks/hardlinks and traversing paths are rejected before "+
			"any write, size and file-count limits apply, and every export lands with a provenance sidecar "+
			"and an audit-trail record."),
		mcp.WithString("task_id", mcp.Required(), mcp.Description("Task ID whose sandbox holds the artifact")),
		mcp.WithString("guest_path", mcp.Required(), mcp.Description("Path to export, relative to the sandbox worktree")),
		mcp.WithString("host_path", mcp.Required(), mcp.Description("Absolute host destination; must not already exist")),
	), s.sandboxExportTool)
}

// sandboxExportTool validates the agent's arguments at the boundary (MCP input
// is untrusted even from a trusted agent), then delegates to the daemon, which
// runs the host-side containment checks and writes the audit record.
// Refs: MGIT-73, SEC-03
func (s *Server) sandboxExportTool(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	taskID, _ := req.GetArguments()["task_id"].(string)
	guestPath, _ := req.GetArguments()["guest_path"].(string)
	hostPath, _ := req.GetArguments()["host_path"].(string)
	if s.sandboxExport == nil {
		return mcp.NewToolResultError("sandbox artifact export is unavailable: this mgit server " +
			"is not wired to a sandbox daemon (run the verb from the CLI: mgit sandbox export)"), nil
	}
	if err := validateTaskID(taskID); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	for name, path := range map[string]string{"guest_path": guestPath, "host_path": hostPath} {
		if err := validatePath(path); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("%s: %s", name, err.Error())), nil
		}
	}
	client, err := s.sandboxExport(ctx)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	res, err := client.ExportArtifact(ctx, taskID,
		model.ArtifactExportRequest{GuestPath: guestPath, HostPath: hostPath})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return jsonResult(res), nil
}
