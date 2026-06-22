package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/hyper-swe/mgit/internal/agentadapter"
)

// sandboxClaudeHookCmd is the Claude Code PreToolUse hook handler wired
// into a worktree's .claude/settings.json (MGIT-11.11.1). Claude invokes
// it for every Bash tool call with the tool input on stdin; it replies on
// stdout with an allow+rewrite decision (route through `mgit run` into the
// task sandbox) when a sandbox is available and the command is not denied,
// or "ask" otherwise. It is hidden because it is machine-driven, not a
// human verb. Refs: MGIT-11.11.1
func sandboxClaudeHookCmd(connect connectFunc) *cobra.Command {
	return &cobra.Command{
		Use:    "claude-hook",
		Short:  "Claude Code PreToolUse hook: route Bash into the task sandbox (internal)",
		Hidden: true,
		Args:   cobra.NoArgs,
		// The hook contract is "exit 0 + JSON decision is authoritative".
		// We always exit 0 and never let cobra print to stdout (which would
		// corrupt the JSON the harness parses).
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runClaudeHook(cmd, connect, os.UserHomeDir)
		},
	}
}

// runClaudeHook reads the PreToolUse payload, computes the routing
// decision, and writes it as JSON. It is best-effort fail-safe: any error
// short of a write failure still emits a valid decision so the harness is
// never left without a reply. Refs: MGIT-11.11.1
func runClaudeHook(cmd *cobra.Command, connect connectFunc, homeFn func() (string, error)) error {
	var in agentadapter.HookInput
	if err := json.NewDecoder(cmd.InOrStdin()).Decode(&in); err != nil {
		// Malformed input: defer to the normal prompt rather than guess.
		return writeJSON(cmd, agentadapter.Decide(in, false, false))
	}
	// Only Bash is rerouted; for any other tool, do not interfere.
	if in.ToolName != "Bash" {
		return writeJSON(cmd, map[string]any{})
	}
	dir := in.Cwd
	if dir == "" {
		if wd, err := os.Getwd(); err == nil {
			dir = wd
		}
	}
	available := sandboxAvailableForDir(cmd.Context(), connect, dir)
	home, _ := homeFn()
	denied := agentadapter.CommandDenied(in.ToolInput.Command, agentadapter.LoadDenyRules(dir, home))
	return writeJSON(cmd, agentadapter.Decide(in, available, denied))
}

// sandboxAvailableForDir reports whether a routable sandbox is bound to
// dir (and the daemon is reachable), reusing the `mgit run` resolution so
// the hook's health check matches exactly what `mgit run` would route to.
// Refs: MGIT-11.11.1, MGIT-11.11.5
func sandboxAvailableForDir(ctx context.Context, connect connectFunc, dir string) bool {
	_, _, _, err := resolveRun(ctx, connect, func() (string, error) { return dir, nil })
	return err == nil
}

// writeJSON encodes v to the command's stdout (the hook reply channel).
func writeJSON(cmd *cobra.Command, v any) error {
	if err := json.NewEncoder(cmd.OutOrStdout()).Encode(v); err != nil {
		return fmt.Errorf("write hook decision: %w", err)
	}
	return nil
}
