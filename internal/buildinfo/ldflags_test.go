package buildinfo

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestLDFlagPath_MatchesTheBuildConfigs guards the one thing about this package
// that no compiler checks.
//
// The version is injected with `-X <import path>.version=...`. That path is a
// STRING in the Makefile and .goreleaser.yaml. Move or rename this package and
// both keep building happily — the -X simply targets a symbol that no longer
// exists, the linker does not complain, and every released binary reports
// "dev (commit: none, built: unknown)". Nothing would catch it until someone
// pasted a useless version string into a bug report.
//
// It also asserts the DAEMON is stamped, not just mgit: before MGIT-83 the
// daemon's build carried only -s -w, so even after adding its --version flag it
// would have reported "dev" in every release. Refs: MGIT-83, MGIT-44
func TestLDFlagPath_MatchesTheBuildConfigs(t *testing.T) {
	root := repoRoot(t)

	makefile := readFile(t, filepath.Join(root, "Makefile"))
	if !strings.Contains(makefile, LDFlagPath) {
		t.Errorf("the Makefile does not mention %q, so `make build` stamps nothing "+
			"and a built binary reports the dev defaults", LDFlagPath)
	}

	goreleaser := readFile(t, filepath.Join(root, ".goreleaser.yaml"))
	// One -X per (binary, field). Three fields across mgit, the linux daemon
	// and the darwin daemon.
	for _, field := range []string{"version", "commit", "date"} {
		want := "-X " + LDFlagPath + "." + field + "="
		if got := strings.Count(goreleaser, want); got != 3 {
			t.Errorf("goreleaser stamps %s.%s on %d builds, want 3 (mgit, "+
				"mgit-sandboxd-linux, mgit-sandboxd-darwin) — an unstamped build "+
				"ships a binary that cannot say which release it is",
				LDFlagPath, field, got)
		}
	}
}

// TestLDFlagPath_IsThisPackagesRealImportPath keeps the constant honest: it is
// only useful as a guard if it names the package it actually lives in.
func TestLDFlagPath_IsThisPackagesRealImportPath(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this test's own source")
	}
	dir := filepath.Dir(thisFile)
	want := "github.com/hyper-swe/mgit/" +
		filepath.ToSlash(mustRel(t, repoRoot(t), dir))
	if LDFlagPath != want {
		t.Errorf("LDFlagPath = %q, but this package lives at %q; the build "+
			"configs are being pointed at a symbol that does not exist", LDFlagPath, want)
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this test's own source")
	}
	// internal/buildinfo/<file> -> module root
	return filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
}

func mustRel(t *testing.T, base, target string) string {
	t.Helper()
	rel, err := filepath.Rel(base, target)
	if err != nil {
		t.Fatalf("relativize %s against %s: %v", target, base, err)
	}
	return rel
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // repo-relative path derived from this test's own location
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}
