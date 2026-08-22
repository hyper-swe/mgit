// Package main is the entry point for the mgit CLI.
// mgit is a checkpointed, sandboxed working substrate for LLM coding agents
// operating within the mtix ecosystem: task-tagged micro-commits in an isolated
// .mgit store over the project's git, with per-task microVM containment.
// Refs: FR-8, NFR-4
package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/hyper-swe/mgit/internal/buildinfo"
)

// Version is the resolved version string (ldflags or module build info),
// consumed by the docs generator. The ldflags-injected vars themselves live in
// internal/buildinfo, which mgit-sandboxd reports from too. Refs: MGIT-40, MGIT-83
var Version = func() string { v, _, _ := buildinfo.Resolve(); return v }()

func main() {
	if err := rootCmd().Execute(); err != nil {
		// A sandbox exec propagates the guest's exit status verbatim; every
		// other failure is exit 1.
		var ee *exitError
		if errors.As(err, &ee) {
			os.Exit(ee.code)
		}
		os.Exit(1)
	}
}

// rootCmd creates the root mgit command.
// Refs: FR-8, MGIT-4.1.1
func rootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:     "mgit",
		Short:   "micro git — a checkpointed, sandboxed working substrate for LLM coding agents",
		Version: versionString(),
		// A RUNTIME failure must not drag the usage/flag dump behind it. A
		// worktree that could not be materialized printed its error and then
		// twenty lines of flags, which pushes the one line that matters off the
		// top of a terminal and implies the user mistyped something.
		//
		// Silencing is armed HERE, at the moment the command's own work
		// begins, rather than on the command struct. Setting SilenceUsage
		// declaratively also suppresses usage for genuine usage errors — bad
		// flags, wrong argument count — which is where the dump is exactly
		// what a reader wants. Cobra validates arguments before this hook
		// runs, so those errors still print usage and everything after it
		// does not. Refs: MGIT-157
		PersistentPreRun: func(cmd *cobra.Command, _ []string) { cmd.SilenceUsage = true },
	}

	root.AddCommand(
		initCmd(),
		commitCmd(),
		logCmd(),
		statusCmd(),
		showCmd(),
		branchCmd(),
		configCmd(),
		rollbackCmd(),
		squashCmd(),
		verifyCmd(),
		auditCmd(),
		addCmd(),
		exportCmd(),
		cherryPickCmd(),
		restoreCmd(),
		checkoutCmd(),
		mergeCmd(),
		gcCmd(),
		importCmd(),
		docsCmd(),
		// Host-only: these drive the sandbox daemon, the host agent shell or
		// the host worktree registry, none of which exist inside a guest.
		// Marked so an agent whose shell is routed into the sandbox gets a
		// diagnosis instead of a socket error (MGIT-61.7).
		hostOnly(worktreeCmd()),
		hostOnly(workCmd()),
		diffCmd(),
		snapshotCmd(),
		hostOnly(doctorCmd(productionSandboxConnect)),
		hostOnly(sandboxCmd()),
		hostOnly(serveCmd()),
		hostOnly(runCmd()),
		versionCmd(),
	)

	return root
}

// openAppFromCwd opens the mgit app from the current working directory.
func openAppFromCwd() (*App, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("get working directory: %w", err)
	}
	return openAppAt(cwd)
}

// openAppAt opens the mgit app for the repo containing start (start or its
// nearest ancestor with a .mgit directory). It backs `mgit serve --project`,
// where the repo must be selected explicitly rather than inferred from cwd
// (the Claude Desktop app launches the MCP server from an arbitrary cwd).
// Refs: MGIT-60
func openAppAt(start string) (*App, error) {
	root, err := findRepoRoot(start)
	if err != nil {
		return nil, err
	}
	return OpenApp(root)
}

// findRepoRoot walks up from start to the nearest ancestor directory that
// contains a .mgit DIRECTORY, mirroring how git locates .git — so mgit commands
// work from any subdirectory of the repo rather than only its root. A plain
// file named .mgit does not count (only the store directory does). Returns an
// error if no .mgit directory is found up to the filesystem root. Refs: MGIT-24
func findRepoRoot(start string) (string, error) {
	dir := start
	for {
		if info, err := os.Stat(filepath.Join(dir, ".mgit")); err == nil && info.IsDir() {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("not an mgit repository (or any parent up to %s): no .mgit directory found", dir)
		}
		dir = parent
	}
}
