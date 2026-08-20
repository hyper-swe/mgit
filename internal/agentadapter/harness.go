package agentadapter

import (
	"fmt"
	"os"
	"path/filepath"
)

// Shell-comment markers for the generated .envrc block (the markdown
// markers would not be comments in a shell file). Refs: MGIT-11.11.3
const (
	envrcBeginMarker = "# BEGIN mgit-sandbox (generated; do not edit)"
	envrcEndMarker   = "# END mgit-sandbox"
)

// cooperativeNotice is the disclaimer every cooperative adapter carries:
// PATH/instruction routing is best-effort, and the hard guarantee is the
// host-anchored land attestation gate. Refs: MGIT-11.11.3, FR-17.6
func cooperativeNotice() string {
	return "These adapters are **cooperative, not enforced**: a determined process can " +
		"bypass the PATH shim. That is acceptable because the hard guarantee is enforced " +
		"host-side at land time — with `require_sandbox` on (the default), any commit not " +
		"produced inside the sandbox has no host attestation and is **refused by " +
		"`mgit sandbox land`**. A bypass is therefore a blocked state, not a silent gap."
}

// routingNarrative is the body describing routing to an agent, WRITTEN FOR THE
// FAMILY THAT WILL READ IT.
//
// It was one shared paragraph until the enforcement tiers diverged, and a
// shared paragraph is now wrong in both directions. Telling Codex and Cursor
// that "routing is cooperative, prepend PATH yourself" understates a guarantee
// their harness hooks now provide — and invites an agent to add a prefix that
// is already applied. Telling the generic lane it is routed overstates one that
// does not exist, which is the defect MGIT-149 was filed for. Each tier gets
// its own words. Refs: MGIT-149, R-H299
func routingNarrative(shimDir string, fam Family) string {
	head := "This task has a hardware-isolated **microVM** sandbox bound to its worktree.\n\n"
	switch fam.Routing {
	case RoutingRouted:
		return head +
			"**Your shell commands are routed into it automatically.** A `" + fam.Config + "` hook rewrites " +
			"every shell call to execute inside the guest, so you do not need to prefix anything or change " +
			"PATH — the harness applies it, not you.\n\n" +
			hookLoadCheck() +
			"A PATH shim at `" + shimDir + "` is installed as well. It is the fallback if the hook is not " +
			"loaded — prepending it routes commands too — but it is advice, not enforcement, so it is a " +
			"stopgap and not a reason to keep going.\n\n" +
			"Run build, install and test commands freely **without asking for approval** once that check " +
			"passes.\n\n" +
			landGateNotice()
	case RoutingBlocked:
		return head +
			"**Prefix every shell command with `mgit run --`.** A `" + fam.Config + "` hook inspects each " +
			"command before it runs; this harness's hook API can refuse a command but cannot rewrite one, so " +
			"anything not routed is **refused** rather than run on the host. A refusal is not a failure — " +
			"re-issue the command as `mgit run -- <command>` and it will run inside the guest.\n\n" +
			"There is also a PATH shim at `" + shimDir + "`; prepending it routes commands the same way.\n\n" +
			hookLoadCheck() +
			landGateNotice()
	default:
		// THE ADVISORY LANE. This is the text R-H299 corrected, kept verbatim in
		// substance: no harness hook exists for an unknown harness, so routing is
		// only as good as this instruction being followed. Refs: MGIT-149, R-H299
		return head +
			"A PATH shim at `" + shimDir + "` routes commands into it.\n\n" +
			"**Routing here is COOPERATIVE, not enforced.** Prepend that directory to PATH and your " +
			"commands run in the guest; if you do not, or you invoke a binary by absolute path, your " +
			"command runs **on the host, uncontained, with no warning**. Harnesses with a command hook " +
			"(Claude Code, Codex, Cursor) are routed or refused by mgit automatically; this one has no such " +
			"hook, so the guarantee is only as good as this instruction being followed.\n\n" +
			"Verify at any time — inside the guest this prints a container hostname and `root`, on " +
			"the host it prints your own machine and user:\n\n" +
			"```\nhostname; whoami\n```\n\n" +
			"With the shim on PATH, run build, install and test commands freely **without asking for " +
			"approval** — those are contained.\n\n" +
			cooperativeNotice()
	}
}

// hookLoadCheck is the verification step the ENFORCED tiers carry.
//
// mgit installs a hook file; it cannot make a harness load one. A harness build
// that predates its own hook support — Codex gained PreToolUse only in v0.114 —
// ignores the file silently, and an agent told "you are routed" would then be
// uncontained while believing the opposite. That is precisely the failure
// MGIT-149 exists to remove, so the claim ships with the check that falsifies
// it, and with what to do when it fails. Refs: MGIT-149
func hookLoadCheck() string {
	return "**Check this first, once per session:**\n\n" +
		"```\nhostname; whoami\n```\n\n" +
		"Inside the guest it prints a container hostname and `root`. If it prints your own machine and " +
		"user instead, the hook is **not loaded** — your harness may be too old to support it, or hooks " +
		"may be disabled. Stop and report that rather than continuing: nothing is containing your " +
		"commands.\n\n"
}

// landGateNotice states the host-anchored backstop for the ENFORCED tiers.
// They do not carry cooperativeNotice's "a determined process can bypass the
// PATH shim" caveat, because for them the PATH shim is not the mechanism.
// Refs: MGIT-149
func landGateNotice() string {
	return "The host-side backstop is unchanged: with `require_sandbox` on (the default), any commit not " +
		"produced inside the sandbox has no host attestation and is **refused by `mgit sandbox land`**."
}

// HookCommand builds the shell command a harness runs for an mgit hook,
// pinned to the ABSOLUTE path of the running mgit binary rather than to
// whatever "mgit" resolves to on the agent's PATH.
//
// The pin is not pedantry: a stale Homebrew mgit winning by PATH order over
// the intended build has already been observed in the field (FEAT-3.68), and
// a routing hook that silently runs the wrong build is a containment defect,
// not an inconvenience. Refs: MGIT-149, FEAT-3.68
func HookCommand(mgitBin, verb string) string {
	return shellQuote(mgitBin) + " sandbox " + verb
}

// RenderCodexDirective renders the AGENTS.md section for Codex. Refs: MGIT-11.11.3
func RenderCodexDirective(shimDir string, env SandboxEnv) string {
	// The POSTURE sections come from the same renderers CLAUDE.md uses, not from
	// a parallel copy. An AGENTS.md-reading agent used to get the routing shims
	// and nothing else: its commands went into the sandbox while it learned
	// nothing about the memory cap, the network mode, or the identical-path
	// mount — on exactly the runs where isolation is real.
	//
	// The costly omission was the caps paragraph. MGIT-95 exists because an
	// agent met an invisible ~1.94 GB ceiling, could not see it, and reshaped a
	// production bundler config to fit a sandbox limit. The line that prevents
	// that recurrence must reach every agent family, so it is SHARED here rather
	// than restated: one renderer means the text cannot drift between families.
	// Refs: MGIT-95, MGIT-11.11.3
	return claudeMdBeginMarker + "\n## Sandbox environment\n\n" +
		routingNarrative(shimDir, mustFamily("codex")) + "\n" +
		renderResources(env) + renderNetwork(env) + claudeMdEndMarker
}

// RenderCursorRule renders the Cursor rules (.mdc) file body. Refs: MGIT-11.11.3
func RenderCursorRule(shimDir string) string {
	return "---\ndescription: mgit sandbox routing\nalwaysApply: true\n---\n\n" +
		"# Sandbox environment\n\n" + routingNarrative(shimDir, mustFamily("cursor")) + "\n"
}

// RenderEnvrc renders the direnv .envrc block that prepends the shim dir to
// PATH for any harness/shell. Refs: MGIT-11.11.3
func RenderEnvrc(shimDir string) string {
	return envrcBeginMarker + "\n" +
		"# Routes commands into this task's mgit microVM sandbox (cooperative;\n" +
		"# the enforced guarantee is the require_sandbox land attestation gate).\n" +
		"export PATH=" + shellQuote(shimDir) + ":\"$PATH\"\n" +
		envrcEndMarker
}

// WriteCodexAdapter installs the routing shims and upserts the AGENTS.md
// directive for Codex. Refs: MGIT-11.11.3
func WriteCodexAdapter(worktreePath, mgitBin string, env SandboxEnv) error {
	if err := InstallShims(ShimDir(worktreePath), mgitBin, DefaultShimCommands()); err != nil {
		return err
	}
	// The ENFORCED half: Codex loads PreToolUse hooks from <repo>/.codex/hooks.json,
	// so this worktree's Bash calls are rewritten into the guest by the harness
	// itself rather than by the agent choosing to honor PATH. Refs: MGIT-149
	if err := WriteCodexHooks(worktreePath, HookCommand(mgitBin, "codex-hook")); err != nil {
		return err
	}
	path := worktreeFilePath(worktreePath, AgentsMdFile)
	return upsertMarkedFile(path, claudeMdBeginMarker, claudeMdEndMarker, RenderCodexDirective(ShimDir(worktreePath), env))
}

// WriteCursorAdapter installs the routing shims and writes the Cursor rules
// file. The .mdc is an mgit-owned generated file, so it is overwritten
// wholesale. Refs: MGIT-11.11.3
func WriteCursorAdapter(worktreePath, mgitBin string) error {
	if err := InstallShims(ShimDir(worktreePath), mgitBin, DefaultShimCommands()); err != nil {
		return err
	}
	// The ENFORCED half: Cursor's beforeShellExecution hook cannot rewrite a
	// command, but it can refuse one, so an uncontained command is blocked
	// rather than run silently on the host. Refs: MGIT-149
	if err := WriteCursorHooks(worktreePath, HookCommand(mgitBin, "cursor-hook")); err != nil {
		return err
	}
	path := worktreeFilePath(worktreePath, CursorRuleFile)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create cursor rules dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(RenderCursorRule(ShimDir(worktreePath))), 0o600); err != nil { //nolint:gosec // worktree-owned generated file
		return fmt.Errorf("write cursor rule: %w", err)
	}
	return nil
}

// WriteGenericAdapter installs the routing shims and upserts a direnv
// .envrc that prepends the shim dir to PATH for any harness. Refs: MGIT-11.11.3
func WriteGenericAdapter(worktreePath, mgitBin string) error {
	if err := InstallShims(ShimDir(worktreePath), mgitBin, DefaultShimCommands()); err != nil {
		return err
	}
	path := worktreeFilePath(worktreePath, EnvrcFile)
	return upsertMarkedFile(path, envrcBeginMarker, envrcEndMarker, RenderEnvrc(ShimDir(worktreePath)))
}

// UpsertAgentsMd re-renders AGENTS.md's marked block with a KNOWN sandbox
// posture. The cooperative adapters are installed at wiring time, before the
// microVM exists, so their first render carries routing but a zero SandboxEnv.
// This is the second pass, called once the sandbox is real — the AGENTS.md
// counterpart of UpsertClaudeMd, so both agent families learn the same facts at
// the same moment. Refs: MGIT-95, MGIT-11.11.3
func UpsertAgentsMd(worktreePath string, env SandboxEnv) error {
	path := worktreeFilePath(worktreePath, AgentsMdFile)
	return upsertMarkedFile(path, claudeMdBeginMarker, claudeMdEndMarker,
		RenderCodexDirective(ShimDir(worktreePath), env))
}

// InstallCooperativeAdapters writes the Codex, Cursor, and generic
// adapters into a worktree (shims are installed once; idempotent).
// Refs: MGIT-11.11.3
func InstallCooperativeAdapters(worktreePath, mgitBin string, env SandboxEnv) error {
	if err := WriteCodexAdapter(worktreePath, mgitBin, env); err != nil {
		return err
	}
	if err := WriteCursorAdapter(worktreePath, mgitBin); err != nil {
		return err
	}
	return WriteGenericAdapter(worktreePath, mgitBin)
}

// mustFamily resolves a family ID that is a compile-time constant in this
// package. A miss is a programming error, not a runtime condition, and the
// advisory tier is the safe fallback: it claims the least. Refs: MGIT-149
func mustFamily(id string) Family {
	if f, ok := LookupFamily(id); ok {
		return f
	}
	return Family{ID: id, Display: id, Routing: RoutingAdvisory, Config: EnvrcFile}
}
