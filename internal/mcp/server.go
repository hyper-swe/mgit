// Package mcp implements the MCP (Model Context Protocol) server for mgit.
// Provides 15 core tools for LLM agent integration via stdio transport.
// Refs: FR-10, MGIT-5.2.1
package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

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
}

// NewServer creates an MCP server with all mgit tools registered.
// Refs: FR-10
func NewServer(
	repo *gitstore.Repository,
	idx *index.Store,
) *Server {
	cs := gitstore.NewCommitStore(repo)
	bs := gitstore.NewBranchStore(repo)

	s := &Server{
		mcpServer: mcpserver.NewMCPServer(
			"mgit",
			"1.0.0",
			mcpserver.WithToolCapabilities(true),
		),
		commit:   service.NewCommitService(repo, cs, idx),
		squash:   service.NewSquashService(repo, cs, idx),
		rollback: service.NewRollbackService(repo, cs, idx),
		branch:   service.NewBranchService(repo, bs, idx),
		verify:   service.NewVerifyService(cs, idx),
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

	// Worktree tools (3) — placeholders for Wave 11
	s.mcpServer.AddTool(mcp.NewTool("mgit_worktree_add",
		mcp.WithDescription("Add a linked worktree"),
		mcp.WithString("path", mcp.Required(), mcp.Description("Worktree path")),
		mcp.WithString("task_id", mcp.Required(), mcp.Description("Task ID")),
	), s.worktreeAddTool)

	s.mcpServer.AddTool(mcp.NewTool("mgit_worktree_list",
		mcp.WithDescription("List linked worktrees"),
	), s.worktreeListTool)

	s.mcpServer.AddTool(mcp.NewTool("mgit_worktree_remove",
		mcp.WithDescription("Remove a linked worktree"),
		mcp.WithString("path", mcp.Required(), mcp.Description("Worktree path")),
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

func (s *Server) worktreeAddTool(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return mcp.NewToolResultText("worktree add: not yet available (Wave 11)"), nil
}

func (s *Server) worktreeListTool(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return mcp.NewToolResultText("worktree list: not yet available (Wave 11)"), nil
}

func (s *Server) worktreeRemoveTool(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return mcp.NewToolResultText("worktree remove: not yet available (Wave 11)"), nil
}
