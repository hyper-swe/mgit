package agentadapter

import (
	"os"
	"path/filepath"
)

// The worktree-relative paths this package generates. They are declared once
// here and used BY the writers below/elsewhere in the package, so the
// declaration cannot drift from what is actually written (a drift-pinning test
// asserts the produced file set equals GeneratedWorktreeFiles). Refs: MGIT-80
const (
	// ClaudeMdFile is the agent knowledge file whose marked mgit block is
	// upserted into every worktree, in every posture. Refs: MGIT-11.11.2
	ClaudeMdFile = "CLAUDE.md"
	// ClaudeSettingsFile is the Claude Code project settings carrying the mgit
	// PreToolUse routing hook. Refs: MGIT-11.11.1
	ClaudeSettingsFile = ".claude/settings.json"
	// AgentsMdFile is the Codex directive file whose marked mgit block is
	// upserted when routing is installed. Refs: MGIT-11.11.3
	AgentsMdFile = "AGENTS.md"
	// CursorRuleFile is the mgit-owned Cursor rule, written wholesale. Refs: MGIT-11.11.3
	CursorRuleFile = ".cursor/rules/mgit-sandbox.mdc"
	// EnvrcFile is the direnv file whose marked mgit block prepends the shim
	// dir to PATH. Refs: MGIT-11.11.3
	EnvrcFile = ".envrc"
)

// GeneratedWorktreeFiles returns the worktree-relative paths mgit generates
// into a worktree for the given containment posture: always the CLAUDE.md
// block, plus the routing wiring when containment was requested or is active
// (the honest-open posture installs no routing — MGIT-47).
//
// It is the provenance list `mgit work` records so a bulk stage cannot sweep
// mgit's own scaffolding into the task branch and the landed patch (MGIT-80).
// It deliberately does NOT include the shims under `.mgit/shims`: everything
// under `.mgit/` is already excluded from staging.
// Refs: MGIT-80, MGIT-47, MGIT-11.11.1, MGIT-11.11.3
func GeneratedWorktreeFiles(contained bool) []string {
	if !contained {
		return []string{ClaudeMdFile}
	}
	return []string{ClaudeMdFile, ClaudeSettingsFile, AgentsMdFile, CursorRuleFile, EnvrcFile}
}

// ExistingGeneratedFiles narrows GeneratedWorktreeFiles to the paths that
// actually exist under worktreePath after the writers have run. Recording only
// what is on disk keeps the provenance claim honest: if an adapter write failed
// (it is best-effort and only warns), mgit does not claim a file it never
// wrote, so a user's own later file of that name is still bulk-staged normally.
// Refs: MGIT-80
func ExistingGeneratedFiles(worktreePath string, contained bool) []string {
	candidates := GeneratedWorktreeFiles(contained)
	present := make([]string, 0, len(candidates))
	for _, rel := range candidates {
		if _, err := os.Lstat(filepath.Join(worktreePath, filepath.FromSlash(rel))); err == nil {
			present = append(present, rel)
		}
	}
	return present
}

// worktreeFilePath resolves a declared worktree-relative generated path to an
// absolute path under worktreePath, translating slashes for the host OS.
func worktreeFilePath(worktreePath, rel string) string {
	return filepath.Join(worktreePath, filepath.FromSlash(rel))
}
