package main

import (
	"encoding/json"
	"os"

	"github.com/spf13/cobra"

	"github.com/hyper-swe/mgit/internal/agentadapter"
)

// sandboxCodexHookCmd is the Codex PreToolUse hook handler wired into a
// worktree's .codex/hooks.json. Codex invokes it for every Bash tool call
// and honors an allow+updatedInput reply, so the command is rewritten to
// run inside the task sandbox — the same enforcement Claude Code gets.
// Hidden because it is machine-driven, not a human verb. Refs: MGIT-149
func sandboxCodexHookCmd(connect connectFunc) *cobra.Command {
	return &cobra.Command{
		Use:           "codex-hook",
		Short:         "Codex PreToolUse hook: route Bash into the task sandbox (internal)",
		Hidden:        true,
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runCodexHook(cmd, connect, os.UserHomeDir)
		},
	}
}

// runCodexHook reads the PreToolUse payload and writes the routing decision.
//
// Unlike the Claude handler, a malformed payload here cannot fall back to
// "ask" — Codex does not honor it — so an undecodable input is DENIED. That
// is the fail-closed direction: a refusal costs the agent a round-trip, while
// the alternative runs an unexamined command on the host. Refs: MGIT-149
func runCodexHook(cmd *cobra.Command, connect connectFunc, homeFn func() (string, error)) error {
	var in agentadapter.HookInput
	if err := json.NewDecoder(cmd.InOrStdin()).Decode(&in); err != nil {
		return writeJSON(cmd, agentadapter.DecideCodex(in, false, false))
	}
	if in.ToolName != "" && in.ToolName != "Bash" {
		return writeJSON(cmd, map[string]any{})
	}
	dir := hookDir(in.Cwd)
	available := sandboxAvailableForDir(cmd.Context(), connect, dir)
	home, _ := homeFn()
	denied := agentadapter.CommandDenied(in.ToolInput.Command, agentadapter.LoadDenyRules(dir, home))
	return writeJSON(cmd, agentadapter.DecideCodex(in, available, denied))
}

// sandboxCursorHookCmd is the Cursor beforeShellExecution hook handler wired
// into a worktree's .cursor/hooks.json. Cursor's response may only permit or
// refuse, so this handler refuses any command that does not already route
// through mgit rather than letting it reach the host. Refs: MGIT-149
func sandboxCursorHookCmd(connect connectFunc) *cobra.Command {
	return &cobra.Command{
		Use:           "cursor-hook",
		Short:         "Cursor beforeShellExecution hook: refuse uncontained commands (internal)",
		Hidden:        true,
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runCursorHook(cmd, connect, os.UserHomeDir)
		},
	}
}

// runCursorHook reads the beforeShellExecution payload and writes the
// permission decision. An undecodable payload is refused, for the same
// fail-closed reason as the Codex handler. Refs: MGIT-149
func runCursorHook(cmd *cobra.Command, connect connectFunc, homeFn func() (string, error)) error {
	var in agentadapter.CursorHookInput
	if err := json.NewDecoder(cmd.InOrStdin()).Decode(&in); err != nil {
		return writeJSON(cmd, agentadapter.DecideCursor(agentadapter.CursorHookInput{}, false, false))
	}
	dir := hookDir(in.Cwd)
	available := sandboxAvailableForDir(cmd.Context(), connect, dir)
	home, _ := homeFn()
	denied := agentadapter.CommandDenied(in.Command, agentadapter.LoadDenyRules(dir, home))
	return writeJSON(cmd, agentadapter.DecideCursor(in, available, denied))
}

// hookDir resolves the directory a hook payload refers to, falling back to
// the process working directory when the harness did not supply one.
func hookDir(cwd string) string {
	if cwd != "" {
		return cwd
	}
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return ""
}
