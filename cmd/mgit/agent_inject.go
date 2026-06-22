package main

import (
	"fmt"
	"io"

	"github.com/hyper-swe/mgit/internal/agentadapter"
	"github.com/hyper-swe/mgit/internal/model"
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

// writeSandboxEnvDoc (re)generates the worktree's CLAUDE.md environment
// section to match a sandbox's current network posture. Called after
// launch so the agent's knowledge layer regenerates on every policy
// change. Best-effort: a write failure warns but never fails the launch.
// Refs: MGIT-11.11.2
func writeSandboxEnvDoc(warn io.Writer, info *model.SandboxInfo) {
	if info == nil || info.WorktreePath == "" {
		return
	}
	env := agentadapter.SandboxEnv{
		WorktreePath: info.WorktreePath,
		NetworkMode:  info.NetworkMode,
		Allowlist:    info.NetworkAllowlist,
	}
	if err := agentadapter.UpsertClaudeMd(info.WorktreePath, env); err != nil {
		_, _ = fmt.Fprintf(warn, "warning: could not update CLAUDE.md sandbox section (%v)\n", err)
	}
}
