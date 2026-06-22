package agentadapter

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ShimDir is the per-worktree directory holding the cooperative routing
// shims. It lives under the worktree so PATH/direnv references are
// absolute and worktree-scoped. Refs: MGIT-11.11.3
func ShimDir(worktreePath string) string {
	return filepath.Join(worktreePath, ".mgit", "shims")
}

// DefaultShimCommands is the set of common build/test/runtime entrypoints
// the cooperative adapters route into the sandbox. Shimming the shell
// entrypoints (bash/sh) routes whole shell commands; shimming the package
// managers/runtimes routes direct invocations. The list is cooperative,
// not exhaustive — the hard guarantee is the land attestation gate, not
// PATH coverage. Refs: MGIT-11.11.3
func DefaultShimCommands() []string {
	return []string{
		"bash", "sh",
		"npm", "npx", "node", "yarn", "pnpm",
		"python", "python3", "pip", "pip3",
		"go", "cargo", "rustc", "make", "cc", "gcc", "clang",
	}
}

// RenderShim renders the shared POSIX shim script. One script is installed
// under each shimmed command name; it routes whatever name it was invoked
// as (`${0##*/}`) plus its arguments through `mgit run`, which execs them
// in the task's microVM sandbox (fail-closed). Refs: MGIT-11.11.3
func RenderShim(mgitBin string) string {
	return "#!/bin/sh\n" +
		"# mgit cooperative routing shim (generated; MGIT-11.11.3). Routes the\n" +
		"# invoked command into the task's microVM sandbox via `mgit run`.\n" +
		"exec " + shellQuote(mgitBin) + " run -- \"${0##*/}\" \"$@\"\n"
}

// InstallShims writes the shared shim script under each command name in
// shimDir (created 0700), making each executable. Command names containing
// a path separator are rejected (they would escape shimDir). Refs: MGIT-11.11.3
func InstallShims(shimDir, mgitBin string, commands []string) error {
	if err := os.MkdirAll(shimDir, 0o700); err != nil {
		return fmt.Errorf("create shim dir: %w", err)
	}
	script := []byte(RenderShim(mgitBin))
	for _, cmd := range commands {
		if cmd == "" || strings.ContainsAny(cmd, `/\`) {
			return fmt.Errorf("invalid shim command name %q", cmd)
		}
		if err := os.WriteFile(filepath.Join(shimDir, cmd), script, 0o700); err != nil { //nolint:gosec // executable shim by design
			return fmt.Errorf("write shim %q: %w", cmd, err)
		}
	}
	return nil
}

// shellQuote single-quotes s for safe embedding in a POSIX shell script,
// escaping any embedded single quote.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
