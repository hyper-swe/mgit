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

// routingNarrative is the shared body describing the sandbox + shim to an
// agent, reused across harness directive formats. Refs: MGIT-11.11.3
func routingNarrative(shimDir string) string {
	return "Your shell commands run inside a hardware-isolated **microVM** sandbox bound to " +
		"this task's worktree, not on the host. They are routed automatically by a PATH shim " +
		"at `" + shimDir + "` (prepend it to PATH). Run build, install, and test commands " +
		"freely **without asking for approval** — they are contained.\n\n" +
		cooperativeNotice()
}

// RenderCodexDirective renders the AGENTS.md section for Codex. Refs: MGIT-11.11.3
func RenderCodexDirective(shimDir string) string {
	return claudeMdBeginMarker + "\n## Sandbox environment\n\n" + routingNarrative(shimDir) + "\n" + claudeMdEndMarker
}

// RenderCursorRule renders the Cursor rules (.mdc) file body. Refs: MGIT-11.11.3
func RenderCursorRule(shimDir string) string {
	return "---\ndescription: mgit sandbox routing\nalwaysApply: true\n---\n\n" +
		"# Sandbox environment\n\n" + routingNarrative(shimDir) + "\n"
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
func WriteCodexAdapter(worktreePath, mgitBin string) error {
	if err := InstallShims(ShimDir(worktreePath), mgitBin, DefaultShimCommands()); err != nil {
		return err
	}
	path := filepath.Join(worktreePath, "AGENTS.md")
	return upsertMarkedFile(path, claudeMdBeginMarker, claudeMdEndMarker, RenderCodexDirective(ShimDir(worktreePath)))
}

// WriteCursorAdapter installs the routing shims and writes the Cursor rules
// file. The .mdc is an mgit-owned generated file, so it is overwritten
// wholesale. Refs: MGIT-11.11.3
func WriteCursorAdapter(worktreePath, mgitBin string) error {
	if err := InstallShims(ShimDir(worktreePath), mgitBin, DefaultShimCommands()); err != nil {
		return err
	}
	dir := filepath.Join(worktreePath, ".cursor", "rules")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create cursor rules dir: %w", err)
	}
	path := filepath.Join(dir, "mgit-sandbox.mdc")
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
	path := filepath.Join(worktreePath, ".envrc")
	return upsertMarkedFile(path, envrcBeginMarker, envrcEndMarker, RenderEnvrc(ShimDir(worktreePath)))
}

// InstallCooperativeAdapters writes the Codex, Cursor, and generic
// adapters into a worktree (shims are installed once; idempotent).
// Refs: MGIT-11.11.3
func InstallCooperativeAdapters(worktreePath, mgitBin string) error {
	if err := WriteCodexAdapter(worktreePath, mgitBin); err != nil {
		return err
	}
	if err := WriteCursorAdapter(worktreePath, mgitBin); err != nil {
		return err
	}
	return WriteGenericAdapter(worktreePath, mgitBin)
}
