// Package main is the entry point for the mgit CLI.
// mgit is a safety-critical micro version control system
// for LLM coding agents operating within the mtix ecosystem.
// Refs: FR-8, NFR-4
package main

import (
	"fmt"
	"os"

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
	)

	return root
}

// openAppFromCwd opens the mgit app from the current working directory.
func openAppFromCwd() (*App, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("get working directory: %w", err)
	}
	return OpenApp(cwd)
}
