package main

import (
	"fmt"
	"runtime/debug"

	"github.com/spf13/cobra"
)

// resolveBuildInfo returns the version, commit, and build date for `mgit version`
// / `mgit --version`. It prefers the values injected at build time via ldflags
// (`-X main.version=...`, set by the Makefile and GoReleaser). When those are
// absent — a plain `go build` or `go install <module>@<tag>` leaves the defaults
// dev/none/unknown — it falls back to the module build info the Go toolchain
// embeds (the module version and the VCS revision/time stamp), so an installed
// binary still reports a real version/commit/date instead of "dev (commit: none,
// built: unknown)". Refs: MGIT-40, MGIT-36
func resolveBuildInfo() (v, c, d string) {
	v, c, d = version, commit, date
	if v != "dev" {
		return v, c, d // ldflags were applied (Makefile / GoReleaser build)
	}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return v, c, d
	}
	if bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		v = bi.Main.Version
	}
	var dirty bool
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			if len(s.Value) >= 12 {
				c = s.Value[:12]
			} else if s.Value != "" {
				c = s.Value
			}
		case "vcs.time":
			if s.Value != "" {
				d = s.Value
			}
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if dirty && c != "none" {
		c += "-dirty"
	}
	return v, c, d
}

// formatVersion renders the one-line version string. Refs: MGIT-40
func formatVersion(v, c, d string) string {
	return fmt.Sprintf("%s (commit: %s, built: %s)", v, c, d)
}

// versionString is the resolved one-line version, shared by `mgit --version`
// (the root command's Version field) and the `mgit version` subcommand. Refs: MGIT-40
func versionString() string {
	return formatVersion(resolveBuildInfo())
}

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
