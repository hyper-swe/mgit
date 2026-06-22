// Package agentadapter generates the cooperative configuration that makes
// LLM coding harnesses (Claude Code, Codex, Cursor) route their task
// commands into the mgit microVM sandbox without harness code changes.
//
// The Claude Code adapter is a PreToolUse hook: it auto-approves a Bash
// command and rewrites it to run through `mgit run` (which routes it into
// the task-bound sandbox, fail-closed), but only when a sandbox is
// available AND the command is not blocked by a user deny rule. When the
// sandbox is unavailable it returns "ask" so the normal permission prompt
// resumes — the command is never silently auto-approved onto the host.
// Refs: FR-17, MGIT-11.11.1
package agentadapter

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Permission decisions a PreToolUse hook may return. "ask" defers to the
// normal permission flow (the user is prompted); it is the fail-closed
// fallback, never a silent host execution. Refs: MGIT-11.11.1
const (
	DecisionAllow = "allow"
	DecisionAsk   = "ask"
)

// hookCommandAllowRule whitelists the rewritten invocation so the routed
// command does not itself trigger a second permission prompt.
const hookCommandAllowRule = "Bash(mgit run:*)"

// HookInput is the subset of the Claude Code PreToolUse stdin payload the
// adapter consumes. Refs: MGIT-11.11.1
type HookInput struct {
	HookEventName string `json:"hook_event_name"`
	ToolName      string `json:"tool_name"`
	Cwd           string `json:"cwd"`
	ToolInput     struct {
		Command string `json:"command"`
	} `json:"tool_input"`
}

// HookOutput is the PreToolUse hook response. Refs: MGIT-11.11.1
type HookOutput struct {
	HookSpecificOutput PreToolUseOutput `json:"hookSpecificOutput"`
}

// PreToolUseOutput carries the permission decision and, on allow, the
// rewritten tool input. Refs: MGIT-11.11.1
type PreToolUseOutput struct {
	HookEventName            string         `json:"hookEventName"`
	PermissionDecision       string         `json:"permissionDecision"`
	PermissionDecisionReason string         `json:"permissionDecisionReason,omitempty"`
	UpdatedInput             map[string]any `json:"updatedInput,omitempty"`
}

// Decide computes the PreToolUse decision for a Bash command. It allows
// (and rewrites to route into the sandbox) only when the sandbox is
// available and the command is not denied; otherwise it asks. A denied
// command is never rewritten, so the rewrite can never launder it past a
// user deny rule. Refs: MGIT-11.11.1
func Decide(in HookInput, sandboxAvailable, denied bool) HookOutput {
	out := PreToolUseOutput{HookEventName: "PreToolUse"}
	switch {
	case !sandboxAvailable:
		out.PermissionDecision = DecisionAsk
		out.PermissionDecisionReason = "mgit sandbox unavailable for this worktree; deferring to the normal permission prompt (fail-closed)"
	case denied:
		out.PermissionDecision = DecisionAsk
		out.PermissionDecisionReason = "command matches a deny rule; deferring to the normal permission flow"
	default:
		out.PermissionDecision = DecisionAllow
		out.PermissionDecisionReason = "routed into the task sandbox via mgit run"
		out.UpdatedInput = map[string]any{"command": RewriteCommand(in.ToolInput.Command)}
	}
	return HookOutput{HookSpecificOutput: out}
}

// RewriteCommand wraps an original shell command so it executes inside the
// task sandbox: `mgit run -- bash -lc '<command>'`. The command is placed
// inside single quotes with embedded single quotes escaped, so arbitrary
// shell text survives intact. Refs: MGIT-11.11.1
func RewriteCommand(command string) string {
	return "mgit run -- bash -lc " + shellQuote(command)
}

// WriteClaudeSettings writes (or merges into) the worktree's
// .claude/settings.json so Claude Code sessions in that worktree route
// Bash commands through hookCommand. Existing user settings are preserved.
// Refs: MGIT-11.11.1
func WriteClaudeSettings(worktreePath, hookCommand string) error {
	dir := filepath.Join(worktreePath, ".claude")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create .claude dir: %w", err)
	}
	path := filepath.Join(dir, "settings.json")
	existing, err := os.ReadFile(path) //nolint:gosec // worktree-owned settings path
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read settings: %w", err)
	}
	merged, err := MergeClaudeSettings(existing, hookCommand)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, merged, 0o600); err != nil {
		return fmt.Errorf("write settings: %w", err)
	}
	return nil
}

// MergeClaudeSettings merges the mgit Bash PreToolUse hook and the
// `mgit run` allow rule into an existing settings.json document (nil/empty
// for a fresh file), preserving every other key and avoiding duplicates on
// repeated merges. Refs: MGIT-11.11.1
func MergeClaudeSettings(existing []byte, hookCommand string) ([]byte, error) {
	doc := map[string]any{}
	if len(existing) > 0 {
		if err := json.Unmarshal(existing, &doc); err != nil {
			return nil, fmt.Errorf("parse existing settings: %w", err)
		}
	}
	mergeBashHook(doc, hookCommand)
	mergeAllowRule(doc, hookCommandAllowRule)
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode settings: %w", err)
	}
	return append(out, '\n'), nil
}

// mergeBashHook ensures exactly one Bash-matched PreToolUse command hook
// invoking hookCommand is present.
func mergeBashHook(doc map[string]any, hookCommand string) {
	hooks := childMap(doc, "hooks")
	pre, _ := hooks["PreToolUse"].([]any)
	for _, e := range pre {
		if entry, ok := e.(map[string]any); ok && entryInvokes(entry, hookCommand) {
			return // already present — idempotent
		}
	}
	hooks["PreToolUse"] = append(pre, map[string]any{
		"matcher": "Bash",
		"hooks":   []any{map[string]any{"type": "command", "command": hookCommand}},
	})
}

// entryInvokes reports whether a PreToolUse entry already runs hookCommand.
func entryInvokes(entry map[string]any, hookCommand string) bool {
	inner, _ := entry["hooks"].([]any)
	for _, h := range inner {
		if hm, ok := h.(map[string]any); ok {
			if cmd, _ := hm["command"].(string); cmd == hookCommand {
				return true
			}
		}
	}
	return false
}

// mergeAllowRule appends rule to permissions.allow if not already present.
func mergeAllowRule(doc map[string]any, rule string) {
	perms := childMap(doc, "permissions")
	allow, _ := perms["allow"].([]any)
	for _, a := range allow {
		if s, _ := a.(string); s == rule {
			return
		}
	}
	perms["allow"] = append(allow, rule)
}

// childMap returns doc[key] as a map, creating it when absent or of the
// wrong type, so callers can write into a stable nested object.
func childMap(doc map[string]any, key string) map[string]any {
	if m, ok := doc[key].(map[string]any); ok {
		return m
	}
	m := map[string]any{}
	doc[key] = m
	return m
}
