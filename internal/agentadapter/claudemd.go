package agentadapter

import (
	"fmt"
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
	// CPUs and MemoryMB are the guest's EFFECTIVE resource caps. They are
	// rendered because their invisibility caused real damage: an agent whose
	// build died against an unseen memory ceiling rewrote the project's
	// production bundler config to fit it. An agent that can READ the ceiling
	// can report it instead of designing around it. Zero omits the section.
	// Refs: R-H212
	CPUs     int
	MemoryMB int
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
		// States what was ESTABLISHED, not what was assumed. The Open posture
		// is selected because `--sandbox` was not passed; nothing probes
		// whether this host has a sandbox backend. The previous wording
		// asserted "no sandbox on this host" and told the reader to install
		// mgit-sandboxd — printed verbatim on a machine where mgit-sandboxd
		// was installed and on PATH. Containment status has to be trustworthy
		// in both directions, so this claims only the request that was (not)
		// made, and names the flag that changes it. Refs: MGIT-47, MGIT-102
		return "Containment: none — no sandbox was requested for this worktree; " +
			"commands run directly on the host (start the task with `mgit work --sandbox` to contain them)"
	}
}

// UpsertClaudeMd writes the generated environment section into the
// worktree's CLAUDE.md, replacing any prior generated block in place
// (regeneration on policy change) and preserving all surrounding user
// content. CLAUDE.md is created if absent. Refs: MGIT-11.11.2
func UpsertClaudeMd(worktreePath string, env SandboxEnv) error {
	path := worktreeFilePath(worktreePath, ClaudeMdFile)
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
	b.WriteString(renderResources(env))
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

// renderOpenBody is the honest-open posture: no sandbox was requested for this
// worktree, so commands run directly on the host. It must NOT claim routing or
// a microVM — that false claim is exactly the MGIT-47 bug.
//
// It must equally not claim the OPPOSITE without having checked. The earlier
// wording said containment was "unavailable here until the sandbox daemon is
// installed", which nothing establishes: the posture is chosen by the absence
// of `--sandbox`, not by any probe of the host. It was printed verbatim on a
// machine with mgit-sandboxd installed and on PATH, sending the reader to
// install what they already had and implying containment was impossible when
// it was one flag away. Refs: MGIT-47, MGIT-102
func renderOpenBody() string {
	var b strings.Builder
	b.WriteString("**No sandbox was requested for this worktree.** ")
	b.WriteString("Run build, install, and test commands **normally — they execute directly on the host**, ")
	b.WriteString("not in a microVM. There is no command routing to worry about.\n\n")
	b.WriteString("Per-task microVM containment is available on demand: start the task with ")
	b.WriteString("`mgit work --sandbox`, or bring one up for this worktree with `mgit sandbox launch`. ")
	b.WriteString("If `mgit-sandboxd` is not installed on this machine, install it and provision a guest ")
	b.WriteString("image first (see docs/INSTALL-SANDBOX.md). Everything else — `mgit commit`, `mgit squash`, ")
	b.WriteString("worktrees, land-by-patch — works without it.\n")
	return b.String()
}

// renderWorkingDiscipline returns the imperative "mgit working discipline"
// subsection injected into every sandboxed agent's CLAUDE.md. It is a static,
// secret-free string (no ambient state read) describing only mgit's own CLI,
// so it cannot leak host secrets and is stable across regenerations.
//
// The commit instruction MUST stage. An earlier version said plain
// `mgit commit -m ...` and never mentioned staging, so an agent following it
// literally produced a branch of empty commits and a hunk-free land patch
// (MGIT-77). It now names the one-command form, `mgit commit -a`.
//
// It also states that `-a` skips mgit's OWN generated scaffolding and how to
// override that by naming a path. That is a description of enforced behavior
// (ADR-013), not an instruction the agent must remember: the exclusion is
// implemented in the staging walk, so an agent that never reads this bullet
// still cannot land mgit's files. Refs: MGIT-29, MGIT-28, MGIT-77, MGIT-80
func renderWorkingDiscipline(c Containment) string {
	return "\n### mgit working discipline\n\n" +
		"This worktree is version-controlled by **mgit** and bound to one task. " +
		disciplineRoutingSentence(c) + "\n\n" +
		"- **Commit after every coherent step, and stage as you commit.** Run " +
		"`mgit commit -a -m \"<what changed>\"` once a step compiles/passes. The `-a` " +
		"is not optional bookkeeping: mgit records only STAGED changes, so a plain " +
		"`mgit commit` after editing files records NOTHING. `-a` stages every change " +
		"(including new files) and commits in one step; use `mgit add <path>` first " +
		"and plain `mgit commit` only when you deliberately want part of your work. " +
		"The task ID is auto-inherited from this worktree, so no `--task-id` is needed. " +
		"Micro-commits are cheap and expected; they are collapsed into one commit at " +
		"land via `mgit squash`, so do not hesitate or batch.\n" +
		"- **A commit that records nothing is refused.** `mgit commit` exits non-zero " +
		"with `nothing to commit` when your tree would be identical to the previous " +
		"commit's — that means your edits were not staged, not that they were saved. " +
		"Fix it by staging (`-a`), never by passing `--allow-empty`.\n" +
		"- **mgit's own generated files are not your work.** `mgit work` wrote this " +
		"worktree's agent scaffolding (this generated CLAUDE.md block, and the agent " +
		"config under `.claude/`), and `mgit commit -a` deliberately SKIPS it, so it " +
		"never lands in the project. You do not need to remember an exception. If you " +
		"genuinely mean to change one of those files — a real project directive, not " +
		"mgit's generated block — stage it by name: `mgit add CLAUDE.md`.\n" +
		"- **Orient before you act.** `mgit status` (which files are staged vs. not), " +
		"`mgit log --oneline` (your steps so far), and `mgit diff` / " +
		"`mgit diff --task-id <ID>` (what changed) keep you grounded between steps.\n" +
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
		return "No sandbox was requested for this worktree, so run commands normally — they execute on the host."
	}
}

// renderResources states the guest's resource ceiling and — the part that
// matters — what to do when a workload does not fit inside it.
//
// The failure this prevents is not a crash but a WRONG REPAIR: a production
// build that exceeded an invisible 2 GB guest had its bundler config rewritten
// to fit, turning a property of the sandbox into permanent product code in a
// customer's repository. The ceiling is stated up front, and the instruction
// is explicit that it is mgit's limit to raise, not the project's to design
// around. Nothing is rendered when the caps are unknown. Refs: R-H212
func renderResources(env SandboxEnv) string {
	if env.CPUs <= 0 && env.MemoryMB <= 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("### Resources\n\n")
	b.WriteString("This guest is capped at")
	if env.CPUs > 0 {
		fmt.Fprintf(&b, " **%d vCPU**", env.CPUs)
	}
	if env.CPUs > 0 && env.MemoryMB > 0 {
		b.WriteString(" /")
	}
	if env.MemoryMB > 0 {
		fmt.Fprintf(&b, " **%d MB of memory**", env.MemoryMB)
	}
	b.WriteString(". That ceiling is a property of THIS SANDBOX, not of this project.\n\n")
	b.WriteString("If a build or test run dies of memory exhaustion — exit 134/137, a killed process, ")
	b.WriteString("or a runtime reporting its heap is full (Node/V8 sizes its heap from the memory it ")
	b.WriteString("can see) — **do not reshape the project to fit this guest**. Changing a bundler, ")
	b.WriteString("compiler, or test-runner configuration to survive a sandbox limit writes a ")
	b.WriteString("posture-dependent fact into product code. Say the sandbox is too small and ask for ")
	b.WriteString("a larger one: `mgit sandbox launch --memory-mb <MB>` (or `mgit work --memory-mb <MB>`). ")
	b.WriteString("A request above the host policy maximum is refused naming that limit, so you will ")
	b.WriteString("never silently receive less than you asked for.\n\n")
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
