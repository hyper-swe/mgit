package agentadapter

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// DecisionDeny refuses a tool call outright. It is Codex's fail-closed
// value: unlike Claude Code, Codex parses "ask" but does not act on it, so
// an unroutable command must be DENIED rather than deferred. Refs: MGIT-149
const DecisionDeny = "deny"

// codexBashMatcher is the regex Codex matches PreToolUse entries against.
// Codex matches tool names by pattern (unlike Claude's literal "Bash"), so
// the anchors matter: an unanchored "Bash" would also match a future tool
// whose name merely contains it. Refs: MGIT-149
const codexBashMatcher = "^Bash$"

// DecideCodex computes the PreToolUse decision for a Codex Bash call.
//
// It mirrors Decide (Claude) in intent — route into the guest when the
// sandbox is healthy and the command is not denied — but DIFFERS in the
// fallback, and the difference is the whole point:
//
//	Claude   unroutable -> "ask": the harness prompts a human. Nothing runs
//	         unattended, so ask is fail-closed there.
//	Codex    unroutable -> "deny". Codex documents "ask" as parsed but not
//	         supported, which means a hook returning it FAILS OPEN and the
//	         command executes on the host — the precise failure this hook
//	         exists to prevent, reintroduced inside the fix.
//
// A denied command is never rewritten, so the rewrite cannot launder a
// command past a user deny rule. Refs: MGIT-149, MGIT-11.11.1
func DecideCodex(in HookInput, sandboxAvailable, denied bool) HookOutput {
	out := PreToolUseOutput{HookEventName: "PreToolUse"}
	switch {
	case !sandboxAvailable:
		out.PermissionDecision = DecisionDeny
		out.PermissionDecisionReason = "mgit sandbox unavailable for this worktree: refusing rather than " +
			"running this command uncontained on the host. Start it with `mgit work --sandbox`, or run " +
			"the command yourself if you intend it to touch the host."
	case denied:
		out.PermissionDecision = DecisionDeny
		out.PermissionDecisionReason = "command matches an mgit deny rule"
	default:
		out.PermissionDecision = DecisionAllow
		out.PermissionDecisionReason = "routed into the task sandbox via mgit run"
		out.UpdatedInput = map[string]any{"command": RewriteCommand(in.ToolInput.Command)}
	}
	return HookOutput{HookSpecificOutput: out}
}

// WriteCodexHooks merges the mgit PreToolUse routing hook into the
// worktree's .codex/hooks.json, preserving any hooks the user already had
// and never duplicating mgit's own on repeated writes.
//
// Codex loads hooks from <repo>/.codex/hooks.json as well as from the
// user's home, and it is the repo-scoped file that makes per-worktree
// enforcement possible: the hook must apply to THIS task's worktree and
// not to everything the user opens. Refs: MGIT-149
func WriteCodexHooks(worktreePath, hookCommand string) error {
	path := worktreeFilePath(worktreePath, CodexHooksFile)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create .codex dir: %w", err)
	}
	existing, err := os.ReadFile(path) //nolint:gosec // worktree-owned generated path
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read codex hooks: %w", err)
	}
	merged, err := MergeCodexHooks(existing, hookCommand)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, merged, 0o600); err != nil {
		return fmt.Errorf("write codex hooks: %w", err)
	}
	return nil
}

// MergeCodexHooks merges the mgit Bash PreToolUse entry into an existing
// hooks.json document (nil/empty for a fresh file), preserving every other
// key and every other hook. Refs: MGIT-149
func MergeCodexHooks(existing []byte, hookCommand string) ([]byte, error) {
	doc := map[string]any{}
	if len(existing) > 0 {
		if err := json.Unmarshal(existing, &doc); err != nil {
			return nil, fmt.Errorf("parse existing codex hooks: %w", err)
		}
	}
	hooks := childMap(doc, "hooks")
	pre, _ := hooks["PreToolUse"].([]any)
	for _, e := range pre {
		if entry, ok := e.(map[string]any); ok && entryInvokes(entry, hookCommand) {
			hooks["PreToolUse"] = pre
			return marshalDoc(doc)
		}
	}
	hooks["PreToolUse"] = append(pre, map[string]any{
		"matcher": codexBashMatcher,
		"hooks": []any{map[string]any{
			"type":          "command",
			"command":       hookCommand,
			"statusMessage": "Routing into the mgit sandbox",
		}},
	})
	return marshalDoc(doc)
}

// marshalDoc encodes a hooks/settings document with a trailing newline.
func marshalDoc(doc map[string]any) ([]byte, error) {
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode hooks: %w", err)
	}
	return append(out, '\n'), nil
}
