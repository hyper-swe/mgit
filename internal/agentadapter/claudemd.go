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

// Containment is the sandbox posture a worktree's agent docs describe. It
// drives whether the CLAUDE.md block claims the shell is routed into a microVM
// (Active), warns that a requested sandbox is not up yet and commands
// fail-closed (Pending), or honestly states that no sandbox exists and commands
// run on the host (Open). The zero value is Active so the live-sandbox
// regeneration path keeps its existing wording. Refs: MGIT-47
type Containment int

const (
	// ContainmentActive means a sandbox is running; the shell is routed into the
	// microVM and commands run contained. Refs: MGIT-47
	ContainmentActive Containment = iota
	// ContainmentPending means containment was requested (`mgit work --sandbox`)
	// but the sandbox is not running yet, so routed commands fail closed (they do
	// NOT run on the host) until it starts. Refs: MGIT-47, NFR-17.6
	ContainmentPending
	// ContainmentOpen means no sandbox is available and none was requested;
	// commands run directly on the host. Honest-open — never claim routing. Refs: MGIT-47
	ContainmentOpen
)

// SandboxEnv is the environment posture rendered into the worktree's
// CLAUDE.md knowledge section. It carries only non-secret facts the agent
// needs to operate without misdiagnosing the sandbox. Refs: MGIT-11.11.2
type SandboxEnv struct {
	WorktreePath string
	NetworkMode  string      // model.NetworkMode{None,Allowlist,Open}
	Allowlist    []string    // allowlist mode only
	Containment  Containment // sandbox posture (default Active) — MGIT-47
}

// ContainmentStatusLine is the single machine-parseable line `mgit work` prints
// so an agent (or the operator) can read the containment posture at a glance.
// It always starts with "Containment: ". Refs: MGIT-47
func ContainmentStatusLine(c Containment) string {
	switch c {
	case ContainmentActive:
		return "Containment: active — commands run inside the task microVM"
	case ContainmentPending:
		return "Containment: requested — the sandbox is not running yet; commands routed through `mgit run` fail closed until you launch it"
	default: // ContainmentOpen
		return "Containment: none — no sandbox on this host; commands run directly on the host (install mgit-sandboxd to enable containment)"
	}
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
	switch env.Containment {
	case ContainmentOpen:
		b.WriteString(renderOpenBody())
	case ContainmentPending:
		b.WriteString(renderPendingBody(env))
	default: // ContainmentActive
		b.WriteString(renderActiveBody(env))
	}
	b.WriteString(renderWorkingDiscipline(env.Containment))
	b.WriteString(claudeMdEndMarker)
	return b.String()
}

// renderActiveBody describes the live-microVM posture (a sandbox is running):
// commands run contained, at the identical path, and may run freely. This is
// the wording the live-sandbox regeneration path emits. Refs: MGIT-11.11.2
func renderActiveBody(env SandboxEnv) string {
	var b strings.Builder
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
	return b.String()
}

// renderPendingBody describes the requested-but-not-running posture. Commands
// routed through `mgit run` fail closed (they do NOT run on the host) until the
// sandbox starts — the fail-closed contract is a feature, so the wording keeps
// it while telling the agent how to bring the sandbox up. Refs: MGIT-47, NFR-17.6
func renderPendingBody(env SandboxEnv) string {
	var b strings.Builder
	b.WriteString("A per-task microVM sandbox was **requested** for this worktree but is **not running yet**. ")
	b.WriteString("Until it starts, commands routed through `mgit run` **fail closed** — they will not run on the host. ")
	b.WriteString("This is deliberate: once containment is requested, mgit never silently falls back to the host.\n\n")
	b.WriteString("To bring the sandbox up (then rerun your command):\n\n")
	if env.WorktreePath != "" {
		fmt.Fprintf(&b, "```\nmgit sandbox launch --worktree %s --image <ref>\n```\n\n", env.WorktreePath)
	} else {
		b.WriteString("```\nmgit sandbox launch --worktree <path> --image <ref>\n```\n\n")
	}
	b.WriteString("If the sandbox daemon is missing, install it (see docs/INSTALL-SANDBOX.md) and relaunch.\n")
	return b.String()
}

// renderOpenBody is the honest-open posture: no sandbox exists and none was
// requested, so commands run directly on the host. It must NOT claim routing or
// a microVM — that false claim is exactly the MGIT-47 bug. Refs: MGIT-47
func renderOpenBody() string {
	var b strings.Builder
	b.WriteString("**No sandbox is active on this machine, and none was requested.** ")
	b.WriteString("Run build, install, and test commands **normally — they execute directly on the host**, ")
	b.WriteString("not in a microVM. There is no command routing to worry about.\n\n")
	b.WriteString("Per-task microVM **containment is unavailable** here until the sandbox daemon is installed. ")
	b.WriteString("To enable it, install `mgit-sandboxd` and provision a guest image (see docs/INSTALL-SANDBOX.md), ")
	b.WriteString("then start the task with `mgit work --sandbox`. Everything else — `mgit commit`, `mgit squash`, ")
	b.WriteString("worktrees, land-by-patch — works without it.\n")
	return b.String()
}

// renderWorkingDiscipline returns the imperative "mgit working discipline"
// subsection injected into every sandboxed agent's CLAUDE.md. It is a static,
// secret-free string (no ambient state read) describing only mgit's own CLI,
// so it cannot leak host secrets and is stable across regenerations.
// Refs: MGIT-29, MGIT-28
func renderWorkingDiscipline(c Containment) string {
	return "\n### mgit working discipline\n\n" +
		"This worktree is version-controlled by **mgit** and bound to one task. " +
		disciplineRoutingSentence(c) + "\n\n" +
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

// disciplineRoutingSentence is the one posture-specific sentence in the working
// discipline: only the Active posture may claim the shell already routes through
// `mgit run`. Pending/Open must not (that claim is the MGIT-47 bug). Refs: MGIT-47
func disciplineRoutingSentence(c Containment) string {
	switch c {
	case ContainmentActive:
		return "Your shell already routes through `mgit run`, so just run commands normally."
	case ContainmentPending:
		return "Once the requested sandbox is running your shell routes through `mgit run`; until then routed commands fail closed (see above)."
	default: // ContainmentOpen
		return "There is no sandbox on this machine, so run commands normally — they execute on the host."
	}
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
