package agentadapter

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Cursor's beforeShellExecution permission values. Unlike Codex, Cursor
// honors "ask" — but mgit does not use it here: an unattended agent run has
// nobody to answer the prompt, and a refusal that names the routed form is
// more actionable than a question. Refs: MGIT-149
const (
	CursorAllow = "allow"
	CursorDeny  = "deny"
)

// cursorHooksVersion is the schema version Cursor requires in hooks.json.
// Omitting it is a parse failure, which would silently disable the hook.
const cursorHooksVersion = 1

// CursorHookInput is the subset of Cursor's beforeShellExecution stdin
// payload the adapter consumes. Refs: MGIT-149
type CursorHookInput struct {
	Command string `json:"command"`
	Cwd     string `json:"cwd"`
}

// CursorHookOutput is the beforeShellExecution response.
//
// The keys are snake_case because Cursor's contract is snake_case — unlike
// the camelCase Claude and Codex use. A camelCase slip here would still
// satisfy Go, still marshal, and be silently ignored by Cursor, which fails
// OPEN. The serialization is pinned by its own test. Refs: MGIT-149
type CursorHookOutput struct {
	Permission   string `json:"permission"`
	UserMessage  string `json:"user_message,omitempty"`
	AgentMessage string `json:"agent_message,omitempty"`
}

// DecideCursor computes the beforeShellExecution decision.
//
// Cursor's hook may only permit or refuse — its response has no field that
// can replace the command — so mgit cannot transparently route the way it
// does for Claude and Codex. What it CAN do is guarantee that nothing
// reaches the host unannounced: a command that does not already route into
// the guest is refused, with the routed form spelled out so the agent can
// re-issue it. That is the RoutingBlocked tier.
//
// `mgit` invocations are permitted because mgit is the substrate and runs on
// the host by design: `mgit run` IS the routing verb, and `mgit commit`,
// `mgit squash` and friends manipulate the store rather than executing the
// task's untrusted code. Everything else must go through the guest.
// Refs: MGIT-149
func DecideCursor(in CursorHookInput, sandboxAvailable, denied bool) CursorHookOutput {
	switch {
	case !sandboxAvailable:
		return CursorHookOutput{
			Permission:  CursorDeny,
			UserMessage: "mgit refused a shell command: the task sandbox is not available.",
			AgentMessage: "The mgit sandbox for this worktree is not available, so this command was refused " +
				"rather than run uncontained on the host. Bring it up (`mgit work --sandbox`), then re-issue " +
				"the command as `mgit run -- <command>`.",
		}
	case denied:
		return CursorHookOutput{
			Permission:   CursorDeny,
			UserMessage:  "mgit refused a shell command: it matches a deny rule.",
			AgentMessage: "This command matches an mgit deny rule and was refused. Running it as `mgit run -- ...` will not bypass the rule.",
		}
	case routesThroughMgit(in.Command):
		return CursorHookOutput{Permission: CursorAllow}
	default:
		return CursorHookOutput{
			Permission:  CursorDeny,
			UserMessage: "mgit refused an uncontained shell command.",
			AgentMessage: "This command would have run on the HOST, outside the task's microVM sandbox. " +
				"Cursor's hook API can refuse a command but cannot rewrite it, so mgit refuses rather than " +
				"letting it reach the host silently. Re-issue it as: mgit run -- " + in.Command,
		}
	}
}

// routesThroughMgit reports whether a command's leading word invokes the mgit
// binary — either the routing verb (`mgit run -- ...`) or another mgit verb,
// which runs host-side by design.
//
// It is deliberately conservative: only the FIRST word is considered, so
// `env PATH=... sh -c '...'`, `cd x && gcc`, and any other construction that
// merely mentions mgit later is refused. Refusing too much costs an agent one
// extra round-trip; permitting too much is a silent host execution, which is
// the defect. Refs: MGIT-149
func routesThroughMgit(command string) bool {
	fields := strings.Fields(strings.TrimSpace(command))
	if len(fields) == 0 {
		return false
	}
	base := fields[0]
	if i := strings.LastIndexAny(base, `/\`); i >= 0 {
		base = base[i+1:]
	}
	return base == "mgit" || base == "mgit.exe"
}

// WriteCursorHooks merges the mgit beforeShellExecution hook into the
// worktree's .cursor/hooks.json, preserving the user's own hooks and never
// duplicating mgit's own on repeated writes. Refs: MGIT-149
func WriteCursorHooks(worktreePath, hookCommand string) error {
	path := worktreeFilePath(worktreePath, CursorHooksFile)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create .cursor dir: %w", err)
	}
	existing, err := os.ReadFile(path) //nolint:gosec // worktree-owned generated path
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read cursor hooks: %w", err)
	}
	merged, err := MergeCursorHooks(existing, hookCommand)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, merged, 0o600); err != nil {
		return fmt.Errorf("write cursor hooks: %w", err)
	}
	return nil
}

// MergeCursorHooks merges the mgit beforeShellExecution entry into an
// existing hooks.json document (nil/empty for a fresh file), preserving all
// other keys and hooks and stamping the required schema version.
// Refs: MGIT-149
func MergeCursorHooks(existing []byte, hookCommand string) ([]byte, error) {
	doc := map[string]any{}
	if len(existing) > 0 {
		if err := json.Unmarshal(existing, &doc); err != nil {
			return nil, fmt.Errorf("parse existing cursor hooks: %w", err)
		}
	}
	doc["version"] = cursorHooksVersion
	hooks := childMap(doc, "hooks")
	entries, _ := hooks["beforeShellExecution"].([]any)
	for _, e := range entries {
		if entry, ok := e.(map[string]any); ok {
			if cmd, _ := entry["command"].(string); cmd == hookCommand {
				return marshalDoc(doc) // already present — idempotent
			}
		}
	}
	hooks["beforeShellExecution"] = append(entries, map[string]any{"command": hookCommand})
	return marshalDoc(doc)
}
