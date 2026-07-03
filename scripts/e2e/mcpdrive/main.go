// Command mcpdrive is the scripted MCP client for the MGIT-48 MCP-posture e2e.
//
// It starts `mgit serve --mcp-only` over stdio and drives the documented MCP
// tool surface the way a real agent would: it lists the tools (asserting the
// full documented set is registered), then calls the worktree tools plus a
// read tool end to end, asserting real behavior — no tool may return the old
// "not yet available" placeholder (MGIT-45), and worktree_add must materialize
// a real worktree that worktree_list then reports.
//
// Usage: mcpdrive
//
//	MGIT_BIN  path to the mgit binary to drive (default: "mgit" on PATH).
//
// It sets up its own throwaway git+mgit repo and runs the server there.
// Exits non-zero (with a diagnostic) on any assertion failure.
//
// Refs: MGIT-48, MGIT-45
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
)

// documentedTools is the tool set the README/MCP docs advertise. The e2e fails
// if the running server registers fewer (drift) — keeping registered == working
// == documented (MGIT-45/50). Refs: MGIT-48
var documentedTools = []string{
	"mgit_commit", "mgit_rollback", "mgit_squash", "mgit_status", "mgit_log",
	"mgit_show", "mgit_branch", "mgit_verify", "mgit_diff", "mgit_export",
	"mgit_audit", "mgit_config",
	"mgit_worktree_add", "mgit_worktree_list", "mgit_worktree_remove",
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "MCP POSTURE E2E: FAIL\n  %v\n", err)
		os.Exit(1)
	}
	fmt.Println("MCP POSTURE E2E: PASS")
}

func run() error {
	mgitBin := os.Getenv("MGIT_BIN")
	if mgitBin == "" {
		mgitBin = "mgit"
	}
	resolved, err := exec.LookPath(mgitBin)
	if err != nil {
		return fmt.Errorf("mgit binary %q not found: %w", mgitBin, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	repo, err := setupRepo(ctx, resolved)
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(repo) }()

	// The stdio server serves the repo in its working directory. NewStdioMCPClient
	// inherits this process's cwd, so run the client from the repo.
	if err := os.Chdir(repo); err != nil {
		return fmt.Errorf("chdir repo: %w", err)
	}

	cli, err := client.NewStdioMCPClient(resolved, os.Environ(), "serve", "--mcp-only")
	if err != nil {
		return fmt.Errorf("start mgit serve --mcp-only: %w", err)
	}
	defer func() { _ = cli.Close() }()

	if err := cli.Start(ctx); err != nil {
		return fmt.Errorf("start client: %w", err)
	}
	if _, err := cli.Initialize(ctx, mcp.InitializeRequest{}); err != nil {
		return fmt.Errorf("initialize handshake: %w", err)
	}
	ok("MCP initialize handshake")

	if err := assertToolSurface(ctx, cli); err != nil {
		return err
	}
	return driveWorktreeLoop(ctx, cli)
}

// setupRepo creates a throwaway git repo with mgit initialized in it.
func setupRepo(ctx context.Context, mgitBin string) (string, error) {
	dir, err := os.MkdirTemp("", "mcpdrive")
	if err != nil {
		return "", err
	}
	steps := [][]string{
		{"git", "init", "-q"},
		{"git", "-c", "user.email=e2e@mgit.local", "-c", "user.name=e2e", "commit", "-q", "--allow-empty", "-m", "init"},
		{mgitBin, "init"},
	}
	for _, s := range steps {
		cmd := exec.CommandContext(ctx, s[0], s[1:]...) //nolint:gosec // fixed argv, e2e helper
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			_ = os.RemoveAll(dir)
			return "", fmt.Errorf("setup %v: %w\n%s", s, err, out)
		}
	}
	return dir, nil
}

// assertToolSurface lists the server's tools and checks the full documented set
// is registered and none carries placeholder text.
func assertToolSurface(ctx context.Context, cli *client.Client) error {
	res, err := cli.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		return fmt.Errorf("tools/list: %w", err)
	}
	have := map[string]bool{}
	for _, t := range res.Tools {
		have[t.Name] = true
		if strings.Contains(strings.ToLower(t.Description), "not yet available") {
			return fmt.Errorf("tool %q advertises placeholder text in its description", t.Name)
		}
	}
	for _, want := range documentedTools {
		if !have[want] {
			return fmt.Errorf("documented tool %q is not registered by the server (doc/impl drift)", want)
		}
	}
	ok(fmt.Sprintf("all %d documented tools registered", len(documentedTools)))
	return nil
}

// driveWorktreeLoop calls the worktree tools end to end through the real server,
// proving they are implemented (not stubs) and consistent with each other.
func driveWorktreeLoop(ctx context.Context, cli *client.Client) error {
	wtPath := filepath.Join(os.TempDir(), fmt.Sprintf("mcpdrive-wt-%d", time.Now().UnixNano()))
	defer func() { _ = os.RemoveAll(wtPath) }()

	addText, err := callText(ctx, cli, "mgit_worktree_add", map[string]any{"path": wtPath, "task_id": "MCP-1"})
	if err != nil {
		return err
	}
	if !strings.Contains(addText, "MCP-1") {
		return fmt.Errorf("worktree_add result did not reference the task: %s", addText)
	}
	if _, statErr := os.Stat(wtPath); statErr != nil {
		return fmt.Errorf("worktree_add did not materialize a real worktree at %s", wtPath)
	}
	ok("mgit_worktree_add materialized a real worktree")

	listText, err := callText(ctx, cli, "mgit_worktree_list", nil)
	if err != nil {
		return err
	}
	if !strings.Contains(listText, "MCP-1") {
		return fmt.Errorf("worktree_list did not report the created worktree: %s", listText)
	}
	ok("mgit_worktree_list reports the worktree")

	if _, err := callText(ctx, cli, "mgit_verify", nil); err != nil {
		return err
	}
	ok("mgit_verify runs through MCP")

	rmText, err := callText(ctx, cli, "mgit_worktree_remove", map[string]any{"path": wtPath})
	if err != nil {
		return err
	}
	ok("mgit_worktree_remove runs: " + strings.TrimSpace(rmText))

	// Error path: a missing required arg must be a structured error, not a crash.
	if err := assertToolError(ctx, cli, "mgit_worktree_add", map[string]any{"task_id": "MCP-2"}); err != nil {
		return err
	}
	ok("mgit_worktree_add rejects missing path as a structured error")
	return nil
}

// callText calls a tool, requiring success, and returns its first text content.
func callText(ctx context.Context, cli *client.Client, name string, args map[string]any) (string, error) {
	req := mcp.CallToolRequest{}
	req.Params.Name = name
	req.Params.Arguments = args
	res, err := cli.CallTool(ctx, req)
	if err != nil {
		return "", fmt.Errorf("call %s: %w", name, err)
	}
	text := firstText(res)
	if res.IsError {
		return "", fmt.Errorf("tool %s returned an error: %s", name, text)
	}
	if strings.Contains(strings.ToLower(text), "not yet available") {
		return "", fmt.Errorf("tool %s returned placeholder text: %s", name, text)
	}
	return text, nil
}

// assertToolError requires a tool call to come back as a structured tool error.
func assertToolError(ctx context.Context, cli *client.Client, name string, args map[string]any) error {
	req := mcp.CallToolRequest{}
	req.Params.Name = name
	req.Params.Arguments = args
	res, err := cli.CallTool(ctx, req)
	if err != nil {
		return fmt.Errorf("call %s: %w", name, err)
	}
	if !res.IsError {
		return fmt.Errorf("tool %s should have returned an error for invalid input", name)
	}
	return nil
}

func firstText(res *mcp.CallToolResult) string {
	for _, c := range res.Content {
		if tc, ok := mcp.AsTextContent(c); ok {
			return tc.Text
		}
	}
	return ""
}

func ok(what string) { fmt.Printf("  ok: %s\n", what) }
