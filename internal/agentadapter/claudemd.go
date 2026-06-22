package agentadapter

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Markers delimiting the mgit-generated CLAUDE.md section so it can be
// regenerated in place on policy change without disturbing user content.
// Refs: MGIT-11.11.2
const (
	claudeMdBeginMarker = "<!-- BEGIN mgit-sandbox (generated; do not edit) -->"
	claudeMdEndMarker   = "<!-- END mgit-sandbox -->"
)

// SandboxEnv is the environment posture rendered into the worktree's
// CLAUDE.md knowledge section. It carries only non-secret facts the agent
// needs to operate without misdiagnosing the sandbox. Refs: MGIT-11.11.2
type SandboxEnv struct {
	WorktreePath string
	NetworkMode  string   // model.NetworkMode{None,Allowlist,Open}
	Allowlist    []string // allowlist mode only
}

// UpsertClaudeMd writes the generated environment section into the
// worktree's CLAUDE.md, replacing any prior generated block in place
// (regeneration on policy change) and preserving all surrounding user
// content. CLAUDE.md is created if absent. Refs: MGIT-11.11.2
func UpsertClaudeMd(worktreePath string, env SandboxEnv) error {
	path := filepath.Join(worktreePath, "CLAUDE.md")
	existing, err := os.ReadFile(path) //nolint:gosec // worktree-owned doc path
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read CLAUDE.md: %w", err)
	}
	updated := upsertSection(string(existing), RenderClaudeMdSection(env))
	if err := os.WriteFile(path, []byte(updated), 0o600); err != nil { //nolint:gosec // worktree-owned doc path
		return fmt.Errorf("write CLAUDE.md: %w", err)
	}
	return nil
}

// upsertSection replaces the marked block in body with section, or appends
// it (separated by a blank line) when no marked block is present.
func upsertSection(body, section string) string {
	start := strings.Index(body, claudeMdBeginMarker)
	end := strings.Index(body, claudeMdEndMarker)
	if start >= 0 && end > start {
		end += len(claudeMdEndMarker)
		return body[:start] + section + body[end:]
	}
	if body == "" {
		return section + "\n"
	}
	return strings.TrimRight(body, "\n") + "\n\n" + section + "\n"
}

// RenderClaudeMdSection renders the marked CLAUDE.md knowledge section
// describing the microVM environment, identical-path mount, network
// posture, and the MGIT-EGRESS-DENIED remedy protocol. It reads no ambient
// state — only the supplied SandboxEnv — so no host secret can leak in.
// Refs: MGIT-11.11.2, ADR-005
func RenderClaudeMdSection(env SandboxEnv) string {
	var b strings.Builder
	b.WriteString(claudeMdBeginMarker)
	b.WriteString("\n## Sandbox environment\n\n")
	b.WriteString("Your shell commands run inside a hardware-isolated **microVM**, not on the host. ")
	b.WriteString("The worktree is mounted at the **identical path**")
	if env.WorktreePath != "" {
		fmt.Fprintf(&b, " (`%s`)", env.WorktreePath)
	}
	b.WriteString(", so cwd, globs, and absolute paths work unchanged. ")
	b.WriteString("Run build, install, and test commands freely **without asking for approval** — ")
	b.WriteString("they are contained and cannot harm the host.\n\n")
	b.WriteString("### Network\n\n")
	b.WriteString(renderNetwork(env))
	b.WriteString("\n### When a connection is blocked\n\n")
	b.WriteString("A denied egress fails fast with a machine-readable line:\n\n")
	b.WriteString("```\nMGIT-EGRESS-DENIED host=<host:port> remedy=<command>\n```\n\n")
	b.WriteString("This is a policy decision, not a transient network error: do not retry blindly. ")
	b.WriteString("Run the exact `remedy=` command (e.g. `mgit sandbox policy request --egress <host:port>`) ")
	b.WriteString("to request the destination; the operator is prompted once, and you can then retry.\n")
	b.WriteString(claudeMdEndMarker)
	return b.String()
}

// renderNetwork describes the egress posture for the agent.
func renderNetwork(env SandboxEnv) string {
	switch env.NetworkMode {
	case "open":
		return "**Open network** (NAT to the host network). All egress is permitted; " +
			"the exfiltration/lateral-movement defenses are OFF for this sandbox.\n"
	case "allowlist":
		var b strings.Builder
		b.WriteString("**Allowlist** egress only. Permitted destinations: ")
		if len(env.Allowlist) == 0 {
			b.WriteString("the host policy defaults (e.g. package registries).")
		} else {
			b.WriteString("`" + strings.Join(env.Allowlist, "`, `") + "` (plus host policy defaults).")
		}
		b.WriteString(" Any other destination is denied.\n")
		return b.String()
	default: // none and unknown both fail safe to "no network"
		return "**No network.** All outbound connections are blocked (vsock control plane only).\n"
	}
}
