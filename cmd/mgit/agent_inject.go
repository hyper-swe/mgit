package main

import (
	"fmt"
	"io"

	"github.com/hyper-swe/mgit/internal/agentadapter"
)

// claudeHookCommand is the shell command Claude Code runs for the Bash
// PreToolUse hook. `mgit` is resolved on PATH inside the agent's
// environment. Refs: MGIT-11.11.1
const claudeHookCommand = "mgit sandbox claude-hook"

// injectAgentAdapters writes the cooperative agent-integration config into
// a freshly created worktree so harness Bash commands route into the task
// sandbox. It is best-effort: a write failure is surfaced as a warning but
// does NOT fail worktree creation (the worktree is already registered),
// and the agent simply falls back to the normal permission prompt — never
// silent host execution. Refs: MGIT-11.11.1
func injectAgentAdapters(warn io.Writer, worktreePath string) {
	if err := agentadapter.WriteClaudeSettings(worktreePath, claudeHookCommand); err != nil {
		_, _ = fmt.Fprintf(warn, "warning: could not write Claude sandbox routing config (%v); "+
			"agent commands will prompt instead of auto-routing\n", err)
	}
}
