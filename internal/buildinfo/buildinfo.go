// Package buildinfo resolves the build metadata every shipped mgit binary
// reports, so `mgit --version` and `mgit-sandboxd --version` cannot disagree
// about the build they came from.
//
// The two binaries ship in ONE archive and are installed together by Homebrew.
// When an operator is diagnosing a sandbox that will not launch, "which build
// is this?" has to have a single answer, and a second copy of this formatting
// is a second thing that can drift. That is not hypothetical in this codebase:
// mgit-sandboxd had no version flag at all until MGIT-83, and the release
// checklist's Gatekeeper smoke invoked one anyway.
//
// The ldflags path is this package, not a main package — see LDFlagPath, whose
// doc comment explains why that string is load-bearing.
// Refs: MGIT-83, MGIT-40, MGIT-44
package buildinfo

import (
	"fmt"
	"runtime/debug"
)

// Injected at build time via -X (the Makefile and GoReleaser both set these).
// The defaults are what a plain `go build` leaves behind, and Resolve falls
// back to the toolchain's own module/VCS stamps rather than reporting them.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// LDFlagPath is the -X prefix a build must use to stamp this package.
//
// It exists to be ASSERTED against the Makefile and .goreleaser.yaml (see
// TestLDFlagPath_MatchesTheBuildConfigs). Moving this package without updating
// those two files is a silent failure: the build still succeeds, the -X simply
// targets a symbol that no longer exists, and every released binary reports
// "dev (commit: none, built: unknown)". Nothing else would catch that until a
// user pasted a version string into a bug report. Refs: MGIT-83
const LDFlagPath = "github.com/hyper-swe/mgit/internal/buildinfo"

// Resolve returns the version, commit, and build date.
//
// It prefers the values injected at build time via ldflags. When those are
// absent — a plain `go build`, or `go install <module>@<tag>` — it falls back
// to the module build info the Go toolchain embeds (module version, VCS
// revision and time), so an installed binary still reports something real
// instead of dev/none/unknown. Refs: MGIT-40, MGIT-36
func Resolve() (v, c, d string) {
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

// Format renders the one-line version string. Refs: MGIT-40
func Format(v, c, d string) string {
	return fmt.Sprintf("%s (commit: %s, built: %s)", v, c, d)
}

// String is the resolved one-line version every binary prints. Refs: MGIT-83
func String() string { return Format(Resolve()) }
