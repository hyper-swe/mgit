package main

import (
	"fmt"
	"io"
	"os"

	"github.com/hyper-swe/mgit/internal/agentadapter"
	"github.com/hyper-swe/mgit/internal/model"
	gitstore "github.com/hyper-swe/mgit/internal/store/git"
)

// claudeHookCommand is the shell command Claude Code runs for the Bash
// PreToolUse hook. `mgit` is resolved on PATH inside the agent's
// environment. Refs: MGIT-11.11.1
const claudeHookCommand = "mgit sandbox claude-hook"

// injectAgentAdapters writes the cooperative agent-integration config into a
// freshly created worktree so harness commands route into the task sandbox: the
// Claude Code PreToolUse hook (MGIT-11.11.1) plus the cooperative
// Codex/Cursor/generic PATH-shim adapters (MGIT-11.11.3).
//
// The routing wiring is installed ONLY when containment was requested or is
// active (contained == true). On a machine where no sandbox was requested and
// none is available, installing the fail-closed shims + the "your shell routes
// through mgit run" hook would make even `echo` fail and mislead the agent
// (MGIT-47) — so nothing is installed and the agent runs commands normally on
// the host (the honest-open posture, made explicit in the CLAUDE.md block).
//
// It is best-effort: a write failure is surfaced as a warning but does NOT fail
// worktree creation. Refs: MGIT-11.11.1, MGIT-11.11.3, MGIT-47
func injectAgentAdapters(warn io.Writer, worktreePath string, contained bool) {
	if !contained {
		// Honest-open: no sandbox requested or available. Install no routing
		// wiring — the agent runs commands directly on the host, as the CLAUDE.md
		// block now states. Never a fail-closed shim that blocks basic commands.
		return
	}
	if err := agentadapter.WriteClaudeSettings(worktreePath, claudeHookCommand); err != nil {
		_, _ = fmt.Fprintf(warn, "warning: could not write Claude sandbox routing config (%v); "+
			"agent commands will prompt instead of auto-routing\n", err)
	}
	if err := agentadapter.InstallCooperativeAdapters(worktreePath, currentMgitBin(), agentadapter.SandboxEnv{}); err != nil {
		_, _ = fmt.Fprintf(warn, "warning: could not install cooperative agent adapters (%v)\n", err)
	}
}

// currentMgitBin resolves the absolute path of the running mgit binary so
// generated shims invoke this exact build; falls back to "mgit" on PATH if
// the path cannot be determined. Refs: MGIT-11.11.3
func currentMgitBin() string {
	if exe, err := os.Executable(); err == nil {
		return exe
	}
	return "mgit"
}

// writeSandboxEnvDoc (re)generates the worktree's CLAUDE.md environment
// section to match a sandbox's current network posture. Called after
// launch so the agent's knowledge layer regenerates on every policy
// change. Best-effort: a write failure warns but never fails the launch.
//
// Writing the block is also what makes CLAUDE.md mgit-generated in that
// worktree, so the provenance is recorded here as well as in `mgit work`
// (MGIT-80) — this path also fires for a worktree created by the plumbing
// `mgit worktree add`, which generates nothing until a sandbox is launched.
// Refs: MGIT-11.11.2, MGIT-80
func writeSandboxEnvDoc(warn io.Writer, info *model.SandboxInfo) {
	if info == nil || info.WorktreePath == "" {
		return
	}
	env := agentadapter.SandboxEnv{
		WorktreePath: info.WorktreePath,
		NetworkMode:  info.NetworkMode,
		Allowlist:    info.NetworkAllowlist,
		// The effective resource ceiling, stated where the agent reads its
		// environment — so a workload that does not fit is reported rather
		// than designed around (R-H212).
		CPUs:     info.CPUs,
		MemoryMB: info.MemoryMB,
	}
	if err := agentadapter.UpsertClaudeMd(info.WorktreePath, env); err != nil {
		_, _ = fmt.Fprintf(warn, "warning: could not update CLAUDE.md sandbox section (%v)\n", err)
		return
	}
	// AGENTS.md gets the SAME posture, from the same renderers. Without this an
	// AGENTS.md-reading agent is routed into the sandbox while learning nothing
	// about it — including the line that stops it reshaping the project to fit a
	// guest it cannot see (MGIT-95). A warning rather than a hard failure: the
	// worktree is already usable, and losing the doc must not lose the sandbox.
	if err := agentadapter.UpsertAgentsMd(info.WorktreePath, env); err != nil {
		_, _ = fmt.Fprintf(warn, "warning: could not update AGENTS.md sandbox section (%v)\n", err)
	}
	if err := gitstore.RecordGeneratedPaths(info.WorktreePath,
		[]string{agentadapter.ClaudeMdFile, agentadapter.AgentsMdFile}); err != nil {
		_, _ = fmt.Fprintf(warn, "warning: could not record mgit's generated CLAUDE.md (%v); "+
			"it may be swept into commits by `mgit commit -a`\n", err)
	}
}
