package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/hyper-swe/mgit/internal/buildinfo"
)

// versionString is the resolved one-line version, shared by `mgit --version`
// (the root command's Version field) and the `mgit version` subcommand.
//
// The resolution and formatting live in internal/buildinfo because
// mgit-sandboxd reports the same build from the same archive and the two must
// not be able to disagree (MGIT-83). Refs: MGIT-40, MGIT-83
func versionString() string { return buildinfo.String() }

// versionCmd implements `mgit version`, printing the resolved build metadata.
// Provided as an explicit subcommand (in addition to the cobra `--version`
// flag) because users reach for `mgit version`. Refs: MGIT-40
func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print mgit version, commit, and build date",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintln(cmd.OutOrStdout(), versionString())
			return err
		},
	}
}
