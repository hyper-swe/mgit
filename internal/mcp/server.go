// Package mcp implements the MCP (Model Context Protocol) server for mgit.
// Provides 15 core tools for LLM agent integration via stdio transport.
// Refs: FR-10, MGIT-5.2.1
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/service"
	gitstore "github.com/hyper-swe/mgit/internal/store/git"
	"github.com/hyper-swe/mgit/internal/store/index"
	"github.com/hyper-swe/mgit/internal/store/lock"
)

// Option configures a Server at construction. Refs: MGIT-46
type Option func(*config)

type config struct {
	locker *lock.Guarder
}

// WithLocker makes every tool call acquire the repo process lock for the
// duration of that call only (per-operation locking), so a long-lived MCP
// server can coexist with the CLI on the same repo instead of holding the
// exclusive lock for its lifetime. A nil locker (the default, e.g. in unit
// tests) leaves tool calls unguarded. Refs: MGIT-46, ADR-009
func WithLocker(g *lock.Guarder) Option {
	return func(c *config) { c.locker = g }
}

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
	audit     *service.AuditService
	diff      *service.DiffService
	config    *service.ConfigService
	sync      *service.SyncService
	wtStore   *gitstore.WorktreeStore
}

// NewServer creates an MCP server with all mgit tools registered.
// Refs: FR-10
func NewServer(
	repo *gitstore.Repository,
	idx *index.Store,
	opts ...Option,
) *Server {
	var cfg config
	for _, opt := range opts {
		opt(&cfg)
	}
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

	// config load is best-effort: a broken config file leaves the config tool to
	// report a clear error rather than failing the whole server. Refs: MGIT-50
	cfgSvc, _ := service.NewConfigService(filepath.Join(repo.MgitDir(), "config.json"))

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
		audit:    audit,
		diff:     service.NewDiffService(gitstore.NewDiffStore(repo), cs, idx),
		config:   cfgSvc,
		sync:     sync,
		wtStore:  ws,
	}

	s.registerTools()

	// Per-operation locking: guard every tool call with the repo process lock so
	// the server never holds it for its lifetime (MGIT-46). Installed only when a
	// locker is wired (serve); unset for direct-handler unit tests.
	if cfg.locker != nil {
		s.mcpServer.Use(lockingMiddleware(cfg.locker))
	}
	return s
}

// lockingMiddleware wraps each tool handler so it runs while holding the repo
// process lock for that call's duration only. If the lock cannot be acquired,
// the call returns a structured tool error naming the holder rather than
// hanging or crashing. Refs: MGIT-46, ADR-009
func lockingMiddleware(g *lock.Guarder) mcpserver.ToolHandlerMiddleware {
	return func(next mcpserver.ToolHandlerFunc) mcpserver.ToolHandlerFunc {
		return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var res *mcp.CallToolResult
			var nerr error
			gerr := g.Guard(func() error {
				res, nerr = next(ctx, req)
				return nil // handler errors propagate out-of-band, not as lock failures
			})
			if gerr != nil {
				// MCP convention: a lock-acquisition failure is surfaced as a tool
				// result with IsError set, not as a Go error (same as every handler
				// above). The gerr is carried in the message.
				//nolint:nilerr // intentional: error is returned as a tool result, not a Go error
				return mcp.NewToolResultError("repo busy: " + gerr.Error()), nil
			}
			return res, nerr
		}
	}
}

// MCPServer returns the underlying mcp-go server (for testing/transport).
func (s *Server) MCPServer() *mcpserver.MCPServer {
	return s.mcpServer
}

// ToolDoc describes a registered MCP tool for documentation generation.
// Refs: MGIT-50
type ToolDoc struct {
	Name        string
	Description string
	Parameters  []string
}

// ToolDocs returns the LIVE registered tool set as documentation records,
// sorted by name (parameters sorted too). `mgit docs generate` builds the MCP
// reference from this, so the generated docs cannot drift from what the server
// actually serves — a new/removed/renamed tool changes the docs automatically.
// Refs: MGIT-50
func (s *Server) ToolDocs() []ToolDoc {
	registered := s.mcpServer.ListTools()
	out := make([]ToolDoc, 0, len(registered))
	for name, st := range registered {
		params := make([]string, 0, len(st.Tool.InputSchema.Properties))
		for p := range st.Tool.InputSchema.Properties {
			params = append(params, p)
		}
		sort.Strings(params)
		out = append(out, ToolDoc{Name: name, Description: st.Tool.Description, Parameters: params})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
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
		mcp.WithDescription("Show differences between commits or for a task"),
		mcp.WithString("commit1", mcp.Description("From commit")),
		mcp.WithString("commit2", mcp.Description("To commit")),
		mcp.WithString("task_id", mcp.Description("Net change for a task (instead of commit1/commit2)")),
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

// jsonResult marshals v to a JSON text tool result, or an error result if
// marshaling fails. Centralizing this keeps the read handlers uniform and
// avoids repeating the (unreachable) marshal-error guard in each. Refs: MGIT-50
func jsonResult(v any) *mcp.CallToolResult {
	data, err := json.Marshal(v)
	if err != nil {
		return mcp.NewToolResultError(err.Error())
	}
	return mcp.NewToolResultText(string(data))
}

func (s *Server) commitTool(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	taskID, _ := req.GetArguments()["task_id"].(string)
	message, _ := req.GetArguments()["message"].(string)
	agentID, _ := req.GetArguments()["agent_id"].(string)
	if err := validateTaskID(taskID); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if err := validateText("message", message); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if err := validateText("agent_id", agentID); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
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
	if err := validateTaskID(taskID); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if err := validateText("reason", reason); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

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
	if err := validateTaskID(taskID); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if err := validateText("message", message); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	squashed, err := s.squash.SquashTask(ctx, service.SquashRequest{
		TaskID: taskID, Message: message, DryRun: dryRun,
	})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(squashed.Message), nil
}

// statusTool reports the working-tree status as JSON, matching `mgit status`:
// it auto-housekeeps the base first (ADR-008) then reads the file status.
// Refs: MGIT-50, FR-8.6
func (s *Server) statusTool(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if err := s.sync.EnsureSynced(ctx); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	files, err := s.wtStore.Status(ctx)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return jsonResult(files), nil
}

func (s *Server) logTool(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	taskID, _ := req.GetArguments()["task_id"].(string)

	if taskID != "" {
		if err := validateTaskID(taskID); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		records, err := s.commit.GetTaskCommits(ctx, taskID)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return jsonResult(records), nil
	}

	commits, err := s.commit.ListCommits(ctx)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return jsonResult(commits), nil
}

func (s *Server) showTool(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	commitID, _ := req.GetArguments()["commit_id"].(string)
	if err := validateToken("commit_id", commitID); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	c, err := s.commit.GetCommit(ctx, commitID)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return jsonResult(c), nil
}

func (s *Server) branchTool(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	taskID, _ := req.GetArguments()["task_id"].(string)

	if taskID != "" {
		if err := validateTaskID(taskID); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
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
	return jsonResult(branches), nil
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
	return jsonResult(issues), nil
}

// diffTool returns a unified diff, matching `mgit diff`: between two commits
// when commit1/commit2 are given, or the net change for a task when task_id is
// given. Refs: MGIT-50, FR-8
func (s *Server) diffTool(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	commit1, _ := req.GetArguments()["commit1"].(string)
	commit2, _ := req.GetArguments()["commit2"].(string)
	taskID, _ := req.GetArguments()["task_id"].(string)
	for name, v := range map[string]string{"commit1": commit1, "commit2": commit2} {
		if v != "" {
			if err := validateToken(name, v); err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
		}
	}
	var (
		diffs []model.FileDiff
		err   error
	)
	switch {
	case commit1 != "" && commit2 != "":
		diffs, err = s.diff.DiffCommits(ctx, commit1, commit2)
	case taskID != "":
		if verr := validateTaskID(taskID); verr != nil {
			return mcp.NewToolResultError(verr.Error()), nil
		}
		diffs, err = s.diff.DiffTask(ctx, taskID)
	default:
		return mcp.NewToolResultError("diff requires either commit1+commit2 or task_id"), nil
	}
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(s.diff.FormatUnified(diffs)), nil
}

func (s *Server) exportTool(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	taskID, _ := req.GetArguments()["task_id"].(string)
	if err := validateTaskID(taskID); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	records, err := s.commit.GetTaskCommits(ctx, taskID)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return jsonResult(records), nil
}

// auditTool returns the append-only audit trail as JSON, optionally filtered by
// task_id, matching `mgit audit`. Refs: MGIT-50, MGIT-20
func (s *Server) auditTool(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	taskID, _ := req.GetArguments()["task_id"].(string)
	if taskID != "" {
		if err := validateTaskID(taskID); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
	}
	entries, err := s.audit.GetAuditLog(service.AuditFilters{TaskID: taskID})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return jsonResult(entries), nil
}

// configTool gets or sets configuration, matching `mgit config`: with a value it
// sets+saves key, with only a key it gets that key, otherwise it returns the
// full config as JSON. Refs: MGIT-50
func (s *Server) configTool(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.config == nil {
		return mcp.NewToolResultError("configuration is unavailable (config file could not be loaded)"), nil
	}
	key, _ := req.GetArguments()["key"].(string)
	value, hasValue := req.GetArguments()["value"].(string)
	if key != "" {
		if err := validateToken("key", key); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
	}
	switch {
	case key != "" && hasValue:
		if err := validateText("value", value); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if err := s.config.Set(key, value); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if err := s.config.Save(); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return mcp.NewToolResultText(fmt.Sprintf("set %s = %s", key, value)), nil
	case key != "":
		v, err := s.config.Get(key)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return jsonResult(map[string]any{key: v}), nil
	default:
		return jsonResult(s.config.GetAll()), nil
	}
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
	if err := validatePath(path); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if err := validateTaskID(taskID); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if err := validateText("agent_id", agentID); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	wt, err := s.worktree.Add(ctx, model.WorktreeAddOptions{
		Path: path, TaskID: taskID, AgentID: agentID, Branch: branch,
	})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return jsonResult(wt), nil
}

// worktreeListTool returns all registered linked worktrees as a JSON array.
// Refs: MGIT-45, FR-16
func (s *Server) worktreeListTool(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	wts, err := s.worktree.List(ctx)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return jsonResult(wts), nil
}

// worktreeRemoveTool removes a linked worktree's registration by path.
// Refs: MGIT-45, FR-16
func (s *Server) worktreeRemoveTool(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	path, _ := req.GetArguments()["path"].(string)
	force, _ := req.GetArguments()["force"].(bool)
	if err := validatePath(path); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if err := s.worktree.Remove(ctx, path, force); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(fmt.Sprintf("Removed worktree %s", path)), nil
}
