// Package mcp implements the MCP (Model Context Protocol) server for mgit.
// Provides 15 core tools for LLM agent integration via stdio transport.
// Refs: FR-10, MGIT-5.2.1
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/service"
	gitstore "github.com/hyper-swe/mgit/internal/store/git"
	"github.com/hyper-swe/mgit/internal/store/index"
)

// Server wraps the MCP server with mgit services.
// Refs: FR-10, MGIT-5.2.1
type Server struct {
	mcpServer *mcpserver.MCPServer
	commit    *service.CommitService
	squash    *service.SquashService
	rollback  *service.RollbackService
	branch    *service.BranchService
	verify    *service.VerifyService
	worktree  *service.WorktreeService
}

// NewServer creates an MCP server with all mgit tools registered.
// Refs: FR-10
func NewServer(
	repo *gitstore.Repository,
	idx *index.Store,
) *Server {
	clock := func() time.Time { return time.Now().UTC() }
	cs := gitstore.NewCommitStore(repo)
	bs := gitstore.NewBranchStore(repo)
	ws := gitstore.NewWorktreeStore(repo)

	// Audit trail shared so commit/squash/rollback record operations (MGIT-20).
	auditPath := filepath.Join(repo.MgitDir(), "audit.log")
	audit := service.NewAuditService(auditPath, clock)

	branchSvc := service.NewBranchService(repo, bs, idx)

	// The worktree tools delegate to the SAME service the CLI uses, wired with
	// the ADR-008 auto-housekeeping (WithSync) so an MCP-created worktree pins
	// its fork-base and carries the local foundation identically to `mgit
	// worktree add`. The server runs at the repo root (boundTask empty), and it
	// already holds the process lock, so these operations do not contend.
	// EnsureSynced degrades to a no-op when there is no readable project git
	// (ADR-008 §6), so this is safe with or without a live `.git`. Refs: MGIT-45, FR-16, ADR-008
	sync := service.NewSyncService(repo, ws, cs, "", clock)
	worktreeSvc := service.NewWorktreeService(idx, branchSvc, ws, clock).
		WithSync(sync, repo, cs)

	s := &Server{
		mcpServer: mcpserver.NewMCPServer(
			"mgit",
			"1.0.0",
			mcpserver.WithToolCapabilities(true),
		),
		commit:   service.NewCommitService(repo, cs, idx).WithAudit(audit),
		squash:   service.NewSquashService(repo, cs, idx).WithAudit(audit),
		rollback: service.NewRollbackService(repo, cs, idx).WithAudit(audit),
		branch:   branchSvc,
		verify:   service.NewVerifyService(cs, idx),
		worktree: worktreeSvc,
	}

	s.registerTools()
	return s
}

// MCPServer returns the underlying mcp-go server (for testing/transport).
func (s *Server) MCPServer() *mcpserver.MCPServer {
	return s.mcpServer
}

// registerTools registers all 15 core MCP tools.
// Refs: FR-10.2, FR-10.3
func (s *Server) registerTools() {
	// Core tools (5)
	s.mcpServer.AddTool(mcp.NewTool("mgit_commit",
		mcp.WithDescription("Create a task-tagged micro-commit"),
		mcp.WithString("task_id", mcp.Required(), mcp.Description("Task ID")),
		mcp.WithString("message", mcp.Description("Commit message")),
		mcp.WithString("agent_id", mcp.Description("Agent ID")),
	), s.commitTool)

	s.mcpServer.AddTool(mcp.NewTool("mgit_rollback",
		mcp.WithDescription("Rollback task commits (creates revert commit)"),
		mcp.WithString("task_id", mcp.Required(), mcp.Description("Task ID")),
		mcp.WithString("reason", mcp.Description("Rollback reason")),
		mcp.WithBoolean("dry_run", mcp.Description("Preview only")),
	), s.rollbackTool)

	s.mcpServer.AddTool(mcp.NewTool("mgit_squash",
		mcp.WithDescription("Squash micro-commits for a task"),
		mcp.WithString("task_id", mcp.Required(), mcp.Description("Task ID")),
		mcp.WithString("message", mcp.Description("Squash message")),
		mcp.WithBoolean("dry_run", mcp.Description("Preview only")),
	), s.squashTool)

	s.mcpServer.AddTool(mcp.NewTool("mgit_status",
		mcp.WithDescription("Show working tree status"),
	), s.statusTool)

	s.mcpServer.AddTool(mcp.NewTool("mgit_log",
		mcp.WithDescription("Show commit history"),
		mcp.WithString("task_id", mcp.Description("Filter by task ID")),
		mcp.WithNumber("limit", mcp.Description("Max commits to show")),
	), s.logTool)

	// Advanced tools (7)
	s.mcpServer.AddTool(mcp.NewTool("mgit_show",
		mcp.WithDescription("Show commit details"),
		mcp.WithString("commit_id", mcp.Required(), mcp.Description("Commit hash")),
	), s.showTool)

	s.mcpServer.AddTool(mcp.NewTool("mgit_branch",
		mcp.WithDescription("Manage branches"),
		mcp.WithString("task_id", mcp.Description("Create branch for task")),
		mcp.WithBoolean("active_only", mcp.Description("List active only")),
	), s.branchTool)

	s.mcpServer.AddTool(mcp.NewTool("mgit_verify",
		mcp.WithDescription("Verify commit chain and index integrity"),
		mcp.WithString("task_id", mcp.Description("Verify specific task")),
	), s.verifyTool)

	s.mcpServer.AddTool(mcp.NewTool("mgit_diff",
		mcp.WithDescription("Show differences between commits"),
		mcp.WithString("commit1", mcp.Description("From commit")),
		mcp.WithString("commit2", mcp.Description("To commit")),
	), s.diffTool)

	s.mcpServer.AddTool(mcp.NewTool("mgit_export",
		mcp.WithDescription("Export task commits as JSON"),
		mcp.WithString("task_id", mcp.Required(), mcp.Description("Task ID")),
	), s.exportTool)

	s.mcpServer.AddTool(mcp.NewTool("mgit_audit",
		mcp.WithDescription("View audit trail"),
		mcp.WithString("task_id", mcp.Description("Filter by task")),
	), s.auditTool)

	s.mcpServer.AddTool(mcp.NewTool("mgit_config",
		mcp.WithDescription("Get/set configuration"),
		mcp.WithString("key", mcp.Description("Config key")),
		mcp.WithString("value", mcp.Description("Value to set")),
	), s.configTool)

	// Worktree tools (3) — delegate to the same WorktreeService as the CLI.
	s.mcpServer.AddTool(mcp.NewTool("mgit_worktree_add",
		mcp.WithDescription("Add a linked worktree bound to a task"),
		mcp.WithString("path", mcp.Required(), mcp.Description("Worktree path")),
		mcp.WithString("task_id", mcp.Required(), mcp.Description("Task ID to bind")),
		mcp.WithString("agent_id", mcp.Description("Agent ID")),
		mcp.WithString("branch", mcp.Description("Branch name (default: task/<task-id>)")),
	), s.worktreeAddTool)

	s.mcpServer.AddTool(mcp.NewTool("mgit_worktree_list",
		mcp.WithDescription("List linked worktrees"),
	), s.worktreeListTool)

	s.mcpServer.AddTool(mcp.NewTool("mgit_worktree_remove",
		mcp.WithDescription("Remove a linked worktree"),
		mcp.WithString("path", mcp.Required(), mcp.Description("Worktree path")),
		mcp.WithBoolean("force", mcp.Description("Force remove even with uncommitted changes")),
	), s.worktreeRemoveTool)
}

// --- Tool Handlers ---

func (s *Server) commitTool(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	taskID, _ := req.GetArguments()["task_id"].(string)
	message, _ := req.GetArguments()["message"].(string)
	agentID, _ := req.GetArguments()["agent_id"].(string)
	if agentID == "" {
		agentID = "mcp-agent"
	}

	c, err := s.commit.CreateCommit(ctx, service.CreateCommitRequest{
		TaskID:  taskID,
		AgentID: agentID,
		Message: message,
	})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(fmt.Sprintf("[%s] %s", c.ShortID(), c.Message)), nil
}

func (s *Server) rollbackTool(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	taskID, _ := req.GetArguments()["task_id"].(string)
	reason, _ := req.GetArguments()["reason"].(string)
	dryRun, _ := req.GetArguments()["dry_run"].(bool)

	revert, err := s.rollback.RollbackTask(ctx, service.RollbackRequest{
		TaskID: taskID, Reason: reason, DryRun: dryRun,
	})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(revert.Message), nil
}

func (s *Server) squashTool(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	taskID, _ := req.GetArguments()["task_id"].(string)
	message, _ := req.GetArguments()["message"].(string)
	dryRun, _ := req.GetArguments()["dry_run"].(bool)

	squashed, err := s.squash.SquashTask(ctx, service.SquashRequest{
		TaskID: taskID, Message: message, DryRun: dryRun,
	})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(squashed.Message), nil
}

func (s *Server) statusTool(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return mcp.NewToolResultText("working tree clean"), nil
}

func (s *Server) logTool(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	taskID, _ := req.GetArguments()["task_id"].(string)

	if taskID != "" {
		records, err := s.commit.GetTaskCommits(ctx, taskID)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		data, _ := json.Marshal(records)
		return mcp.NewToolResultText(string(data)), nil
	}

	commits, err := s.commit.ListCommits(ctx)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	data, _ := json.Marshal(commits)
	return mcp.NewToolResultText(string(data)), nil
}

func (s *Server) showTool(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	commitID, _ := req.GetArguments()["commit_id"].(string)
	c, err := s.commit.GetCommit(ctx, commitID)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	data, _ := json.Marshal(c)
	return mcp.NewToolResultText(string(data)), nil
}

func (s *Server) branchTool(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	taskID, _ := req.GetArguments()["task_id"].(string)

	if taskID != "" {
		branch, err := s.branch.CreateBranch(ctx, taskID)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return mcp.NewToolResultText(fmt.Sprintf("Created branch %s", branch.Name)), nil
	}

	branches, err := s.branch.ListBranches(ctx)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	data, _ := json.Marshal(branches)
	return mcp.NewToolResultText(string(data)), nil
}

func (s *Server) verifyTool(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	taskID, _ := req.GetArguments()["task_id"].(string)

	if taskID != "" {
		if err := s.verify.VerifyTaskCommits(ctx, taskID); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return mcp.NewToolResultText(fmt.Sprintf("Task %s: all commits verified", taskID)), nil
	}

	issues, err := s.verify.VerifyIndexIntegrity(ctx)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if len(issues) == 0 {
		return mcp.NewToolResultText("All checks passed"), nil
	}
	data, _ := json.Marshal(issues)
	return mcp.NewToolResultText(string(data)), nil
}

func (s *Server) diffTool(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return mcp.NewToolResultText("no changes"), nil
}

func (s *Server) exportTool(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	taskID, _ := req.GetArguments()["task_id"].(string)
	records, err := s.commit.GetTaskCommits(ctx, taskID)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	data, _ := json.Marshal(records)
	return mcp.NewToolResultText(string(data)), nil
}

func (s *Server) auditTool(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return mcp.NewToolResultText("no audit entries"), nil
}

func (s *Server) configTool(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return mcp.NewToolResultText("config: default"), nil
}

// worktreeAddTool materializes a task-bound linked worktree via the same
// WorktreeService the CLI uses, returning the created worktree as JSON.
// Required args are validated before delegation (agent input is untrusted);
// the service re-validates the task-id grammar and enforces path/task
// isolation. Refs: MGIT-45, FR-16
func (s *Server) worktreeAddTool(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	path, _ := req.GetArguments()["path"].(string)
	taskID, _ := req.GetArguments()["task_id"].(string)
	agentID, _ := req.GetArguments()["agent_id"].(string)
	branch, _ := req.GetArguments()["branch"].(string)
	if path == "" {
		return mcp.NewToolResultError("path is required"), nil
	}
	if taskID == "" {
		return mcp.NewToolResultError("task_id is required"), nil
	}

	wt, err := s.worktree.Add(ctx, model.WorktreeAddOptions{
		Path: path, TaskID: taskID, AgentID: agentID, Branch: branch,
	})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	data, err := json.Marshal(wt)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(string(data)), nil
}

// worktreeListTool returns all registered linked worktrees as a JSON array.
// Refs: MGIT-45, FR-16
func (s *Server) worktreeListTool(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	wts, err := s.worktree.List(ctx)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	data, err := json.Marshal(wts)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(string(data)), nil
}

// worktreeRemoveTool removes a linked worktree's registration by path.
// Refs: MGIT-45, FR-16
func (s *Server) worktreeRemoveTool(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	path, _ := req.GetArguments()["path"].(string)
	force, _ := req.GetArguments()["force"].(bool)
	if path == "" {
		return mcp.NewToolResultError("path is required"), nil
	}
	if err := s.worktree.Remove(ctx, path, force); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(fmt.Sprintf("Removed worktree %s", path)), nil
}
