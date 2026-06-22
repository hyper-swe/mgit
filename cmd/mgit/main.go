// Package main is the entry point for the mgit CLI.
// mgit is a safety-critical micro version control system
// for LLM coding agents operating within the mtix ecosystem.
// Refs: FR-8, NFR-4
package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

// Build-time variables injected via ldflags.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// Version returns the formatted version string.
var Version = version

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
		Short:   "micro git — safety-critical version control for LLM agents",
		Version: fmt.Sprintf("%s (commit: %s, built: %s)", version, commit, date),
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
		worktreeCmd(),
		diffCmd(),
		sandboxCmd(),
		serveCmd(),
		runCmd(),
	)

	return root
}

// openAppFromCwd opens the mgit app from the current working directory.
func openAppFromCwd() (*App, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("get working directory: %w", err)
	}
	root, err := findRepoRoot(cwd)
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
