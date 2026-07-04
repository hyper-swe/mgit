// Command mcpdrive is the scripted MCP client for the MGIT-48 MCP-posture e2e.
//
// It starts `mgit serve --mcp-only` over stdio and drives the documented MCP
// tool surface the way a real agent would: it lists the tools (asserting the
// full documented set is registered), calls the worktree tools end to end,
// then drives EVERY remaining core tool (commit, status, log, show, diff,
// branch, verify, audit, export, config, squash, rollback) with real content
// assertions — no tool may return the old "not yet available" placeholder
// (MGIT-45), worktree_add must materialize a real worktree that worktree_list
// then reports, and the commit→read→squash→rollback loop must be consistent
// across tools (the hash from mgit_commit must resolve via mgit_show, appear
// in mgit_log/mgit_export/mgit_audit, etc.).
//
// Staging note: there is deliberately no mgit_add MCP tool, so the core drive
// stages files via the CLI (`mgit add`) against the same repo — exactly the
// CLI/MCP coexistence that per-operation locking (MGIT-46) exists to support.
// Squash --to-git / --to-main are CLI-only by design (docs/MCP-PARITY.md), so
// mgit_squash is driven to its documented scope: the task-branch artifact.
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
	"encoding/json"
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
	if err := driveWorktreeLoop(ctx, cli); err != nil {
		return err
	}
	return driveCoreTools(ctx, cli, resolved, repo)
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

// driveCoreTools drives every remaining documented tool through the real
// server, in dependency order, asserting returned content (not just no-error):
// config set/get → stage+commit → status/log/show/diff reads → branch/verify/
// audit/export → squash artifact → rollback revert → hostile-input rejections.
func driveCoreTools(ctx context.Context, cli *client.Client, mgitBin, repo string) error {
	if err := driveConfig(ctx, cli); err != nil {
		return err
	}
	hash, err := driveCommitAndReads(ctx, cli, mgitBin, repo)
	if err != nil {
		return err
	}
	if err := driveProvenanceTools(ctx, cli, hash); err != nil {
		return err
	}
	if err := driveSquash(ctx, cli); err != nil {
		return err
	}
	if err := driveRollback(ctx, cli, mgitBin, repo); err != nil {
		return err
	}
	return driveHostileInputs(ctx, cli)
}

// driveConfig round-trips a real config value through mgit_config set then get.
func driveConfig(ctx context.Context, cli *client.Client) error {
	setText, err := callText(ctx, cli, "mgit_config", map[string]any{"key": "project.name", "value": "mcpdrive-e2e"})
	if err != nil {
		return err
	}
	if !strings.Contains(setText, "project.name") || !strings.Contains(setText, "mcpdrive-e2e") {
		return fmt.Errorf("mgit_config set did not confirm the assignment: %s", setText)
	}
	getText, err := callText(ctx, cli, "mgit_config", map[string]any{"key": "project.name"})
	if err != nil {
		return err
	}
	if !strings.Contains(getText, "mcpdrive-e2e") {
		return fmt.Errorf("mgit_config get did not return the value just set: %s", getText)
	}
	ok("mgit_config set/get round-trips project.name")
	return nil
}

// driveCommitAndReads stages a real file (via the CLI — staging has no MCP
// tool), checks status, commits it through mgit_commit, and asserts the read
// tools (log, show, diff) all agree on the result. Returns the new commit's
// hash prefix parsed from the mgit_commit response.
//
// The stage -> status -> commit order is deliberate: this is the NATURAL
// agent flow, and it regresses MGIT-56 — the status-time ADR-008 resync used
// to absorb the staged file into the [mgit-sync] base, silently emptying this
// task's net diff (asserted non-empty in assertLogShowDiff below).
func driveCommitAndReads(ctx context.Context, cli *client.Client, mgitBin, repo string) (string, error) {
	if err := stageFile(ctx, mgitBin, repo, "feature.txt", "hello from mcpdrive\n"); err != nil {
		return "", err
	}
	statusText, err := callText(ctx, cli, "mgit_status", nil)
	if err != nil {
		return "", err
	}
	if !strings.Contains(statusText, "feature.txt") {
		return "", fmt.Errorf("mgit_status did not report the staged file pre-commit: %s", statusText)
	}
	ok("mgit_status between stage and first commit (natural order, MGIT-56)")

	commitText, err := callText(ctx, cli, "mgit_commit", map[string]any{
		"task_id": "MCP-CORE-1", "message": "add feature file", "agent_id": "mcpdrive-e2e",
	})
	if err != nil {
		return "", err
	}
	if !strings.Contains(commitText, "MCP-CORE-1") || !strings.Contains(commitText, "add feature file") {
		return "", fmt.Errorf("mgit_commit result missing task tag or message: %s", commitText)
	}
	hash, err := parseCommitHash(commitText)
	if err != nil {
		return "", err
	}
	ok("mgit_commit created task-tagged commit " + hash)

	if err := assertLogShowDiff(ctx, cli, hash); err != nil {
		return "", err
	}
	return hash, nil
}

// assertLogShowDiff checks log/show/diff consistency for the commit just made.
func assertLogShowDiff(ctx context.Context, cli *client.Client, hash string) error {
	logText, err := callText(ctx, cli, "mgit_log", nil)
	if err != nil {
		return err
	}
	if !strings.Contains(logText, "add feature file") {
		return fmt.Errorf("mgit_log does not show the commit message: %s", logText)
	}
	taskLog, err := callText(ctx, cli, "mgit_log", map[string]any{"task_id": "MCP-CORE-1"})
	if err != nil {
		return err
	}
	if !strings.Contains(taskLog, hash) {
		return fmt.Errorf("task-filtered mgit_log does not include commit %s: %s", hash, taskLog)
	}
	ok("mgit_log shows the commit (full and task-filtered)")

	showText, err := callText(ctx, cli, "mgit_show", map[string]any{"commit_id": hash})
	if err != nil {
		return err
	}
	var shown struct {
		CommitID string `json:"commit_id"`
		ParentID string `json:"parent_id"`
		Message  string `json:"message"`
	}
	if err := json.Unmarshal([]byte(showText), &shown); err != nil {
		return fmt.Errorf("mgit_show returned non-JSON: %w: %s", err, showText)
	}
	if !strings.HasPrefix(shown.CommitID, hash) || !strings.Contains(shown.Message, "add feature file") {
		return fmt.Errorf("mgit_show hash/message mismatch for %s: %s", hash, showText)
	}
	ok("mgit_show resolves the hash with matching message")

	taskDiff, err := callText(ctx, cli, "mgit_diff", map[string]any{"task_id": "MCP-CORE-1"})
	if err != nil {
		return err
	}
	if !strings.Contains(taskDiff, "feature.txt") {
		return fmt.Errorf("mgit_diff (task) does not show feature.txt: %s", taskDiff)
	}
	pairDiff, err := callText(ctx, cli, "mgit_diff", map[string]any{
		"commit1": shown.ParentID, "commit2": shown.CommitID,
	})
	if err != nil {
		return err
	}
	if !strings.Contains(pairDiff, "feature.txt") {
		return fmt.Errorf("mgit_diff (commit pair) does not show feature.txt: %s", pairDiff)
	}
	ok("mgit_diff shows the change (task form and commit-pair form)")
	return nil
}

// driveProvenanceTools drives branch, verify, audit, and export against the
// MCP-CORE-1 commit, asserting each returns real provenance.
func driveProvenanceTools(ctx context.Context, cli *client.Client, hash string) error {
	createText, err := callText(ctx, cli, "mgit_branch", map[string]any{"task_id": "MCP-CORE-2"})
	if err != nil {
		return err
	}
	if !strings.Contains(createText, "MCP-CORE-2") {
		return fmt.Errorf("mgit_branch create did not name the task branch: %s", createText)
	}
	listText, err := callText(ctx, cli, "mgit_branch", nil)
	if err != nil {
		return err
	}
	if !strings.Contains(listText, "MCP-CORE-2") {
		return fmt.Errorf("mgit_branch list does not report the created branch: %s", listText)
	}
	ok("mgit_branch creates and lists task/MCP-CORE-2")

	verifyText, err := callText(ctx, cli, "mgit_verify", map[string]any{"task_id": "MCP-CORE-1"})
	if err != nil {
		return err
	}
	if !strings.Contains(verifyText, "verified") {
		return fmt.Errorf("mgit_verify (task) did not confirm verification: %s", verifyText)
	}
	ok("mgit_verify verifies the task's commits")

	auditText, err := callText(ctx, cli, "mgit_audit", map[string]any{"task_id": "MCP-CORE-1"})
	if err != nil {
		return err
	}
	if !strings.Contains(auditText, "MCP-CORE-1") || !strings.Contains(auditText, "CREATE_COMMIT") {
		return fmt.Errorf("mgit_audit missing the CREATE_COMMIT entry for the task: %s", auditText)
	}
	ok("mgit_audit shows the CREATE_COMMIT trail entry")

	exportText, err := callText(ctx, cli, "mgit_export", map[string]any{"task_id": "MCP-CORE-1"})
	if err != nil {
		return err
	}
	if !strings.Contains(exportText, "MCP-CORE-1") || !strings.Contains(exportText, hash) {
		return fmt.Errorf("mgit_export payload missing task id or commit %s: %s", hash, exportText)
	}
	ok("mgit_export payload references the task and its commit")
	return nil
}

// driveSquash squashes MCP-CORE-1 and asserts the task-branch artifact exists.
// Scope note: mgit_squash produces the task-isolated artifact on task/<id>;
// --to-git / --to-main landing is CLI-only by design (docs/MCP-PARITY.md).
func driveSquash(ctx context.Context, cli *client.Client) error {
	squashText, err := callText(ctx, cli, "mgit_squash", map[string]any{
		"task_id": "MCP-CORE-1", "message": "squash feature work",
	})
	if err != nil {
		return err
	}
	if !strings.Contains(squashText, "squash feature work") || !strings.Contains(squashText, "MCP-CORE-1") {
		return fmt.Errorf("mgit_squash result missing message or micro-commit summary: %s", squashText)
	}
	listText, err := callText(ctx, cli, "mgit_branch", nil)
	if err != nil {
		return err
	}
	if !strings.Contains(listText, "task/MCP-CORE-1") {
		return fmt.Errorf("squash artifact branch task/MCP-CORE-1 not reported by mgit_branch: %s", listText)
	}
	ok("mgit_squash produced the task/MCP-CORE-1 artifact branch")
	return nil
}

// driveRollback commits a second task and rolls it back, asserting the
// append-only revert commit appears in mgit_log AND that the rollback
// restored content (MGIT-54): the task-added file is gone from disk after.
func driveRollback(ctx context.Context, cli *client.Client, mgitBin, repo string) error {
	if err := stageFile(ctx, mgitBin, repo, "rollme.txt", "to be rolled back\n"); err != nil {
		return err
	}
	statusText, err := callText(ctx, cli, "mgit_status", nil)
	if err != nil {
		return err
	}
	if !strings.Contains(statusText, "rollme.txt") {
		return fmt.Errorf("mgit_status did not report the staged file: %s", statusText)
	}
	ok("mgit_status reports the staged file before commit")

	if _, err := callText(ctx, cli, "mgit_commit", map[string]any{
		"task_id": "MCP-CORE-3", "message": "add rollme file", "agent_id": "mcpdrive-e2e",
	}); err != nil {
		return err
	}
	statusText, err = callText(ctx, cli, "mgit_status", nil)
	if err != nil {
		return err
	}
	if strings.Contains(statusText, "rollme.txt") {
		return fmt.Errorf("mgit_status still reports rollme.txt after commit (not clean): %s", statusText)
	}
	ok("mgit_status is clean of the file after commit")
	rbText, err := callText(ctx, cli, "mgit_rollback", map[string]any{
		"task_id": "MCP-CORE-3", "reason": "e2e revert",
	})
	if err != nil {
		return err
	}
	if _, statErr := os.Stat(filepath.Join(repo, "rollme.txt")); !os.IsNotExist(statErr) {
		return fmt.Errorf("rollback must remove the task-added file from disk (MGIT-54); stat err: %w", statErr)
	}
	ok("mgit_rollback restored the working tree (task file removed, MGIT-54)")
	if !strings.Contains(rbText, "Revert") || !strings.Contains(rbText, "MCP-CORE-3") {
		return fmt.Errorf("mgit_rollback result is not a revert for the task: %s", rbText)
	}
	logText, err := callText(ctx, cli, "mgit_log", nil)
	if err != nil {
		return err
	}
	if !strings.Contains(logText, "Revert: e2e revert") {
		return fmt.Errorf("revert commit missing from mgit_log after rollback: %s", logText)
	}
	ok("mgit_rollback created an append-only revert commit visible in mgit_log")
	return nil
}

// driveHostileInputs asserts structured tool errors on non-worktree tools.
func driveHostileInputs(ctx context.Context, cli *client.Client) error {
	if err := assertToolError(ctx, cli, "mgit_commit", map[string]any{"task_id": "not a task!!"}); err != nil {
		return err
	}
	ok("mgit_commit rejects malformed task id as a structured error")

	if err := assertToolError(ctx, cli, "mgit_config", map[string]any{"key": "project.nope"}); err != nil {
		return err
	}
	ok("mgit_config rejects an unknown key as a structured error")

	if err := assertToolError(ctx, cli, "mgit_show", map[string]any{"commit_id": "deadbeefdeadbeef"}); err != nil {
		return err
	}
	ok("mgit_show rejects an unknown commit hash as a structured error")
	return nil
}

// stageFile writes a file into the repo and stages it via the CLI (`mgit add`).
// Staging deliberately has no MCP tool; per-operation locking (MGIT-46) is what
// makes this CLI call safe against the running server.
func stageFile(ctx context.Context, mgitBin, repo, name, content string) error {
	if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	cmd := exec.CommandContext(ctx, mgitBin, "add", name) //nolint:gosec // fixed argv, e2e helper
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("mgit add %s: %w\n%s", name, err, out)
	}
	return nil
}

// parseCommitHash extracts the abbreviated hash from an mgit_commit result of
// the form "[abcdef12] [MGIT:TASK] message".
func parseCommitHash(text string) (string, error) {
	open := strings.Index(text, "[")
	closeIdx := strings.Index(text, "]")
	if open != 0 || closeIdx < 7 {
		return "", fmt.Errorf("mgit_commit result has no leading [hash]: %s", text)
	}
	return text[open+1 : closeIdx], nil
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
