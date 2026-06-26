package agentadapter

import (
	"fmt"
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
	return upsertMarkedFile(path, claudeMdBeginMarker, claudeMdEndMarker, RenderClaudeMdSection(env))
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
	b.WriteString(renderWorkingDiscipline())
	b.WriteString(claudeMdEndMarker)
	return b.String()
}

// renderWorkingDiscipline returns the imperative "mgit working discipline"
// subsection injected into every sandboxed agent's CLAUDE.md. It is a static,
// secret-free string (no ambient state read) describing only mgit's own CLI,
// so it cannot leak host secrets and is stable across regenerations.
// Refs: MGIT-29, MGIT-28
func renderWorkingDiscipline() string {
	return "\n### mgit working discipline\n\n" +
		"This worktree is version-controlled by **mgit** and bound to one task. " +
		"Your shell already routes through `mgit run`, so just run commands normally.\n\n" +
		"- **Commit after every coherent step.** Run `mgit commit -m \"<what changed>\"` " +
		"once a step compiles/passes — the task ID is auto-inherited from this worktree, " +
		"so no `--task-id` is needed. Micro-commits are cheap and expected; they are " +
		"collapsed into one commit at land via `mgit squash`, so do not hesitate or batch.\n" +
		"- **Orient before you act.** `mgit status` (working tree), `mgit log --oneline` " +
		"(your steps so far), and `mgit diff` / `mgit diff --task-id <ID>` (what changed) " +
		"keep you grounded between steps.\n" +
		"- **Course-correct, don't restart.** When a prior decision proves wrong, return to " +
		"that point instead of rewriting from scratch: `mgit rollback --commit <hash>` " +
		"(append-only revert) or `mgit checkout -b <branch>` to fork a new line from a good " +
		"commit, then `mgit cherry-pick <hash>` to salvage the still-good work from the old " +
		"line. The operator or a review agent may direct these steps.\n"
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
