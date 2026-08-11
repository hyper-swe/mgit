package buildinfo

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// stampRe matches any `-X <import path>.<field>=` in a build config or script.
// The captured path is what the linker will look for; if it does not name this
// package, the stamp silently does nothing.
var stampRe = regexp.MustCompile(`-X\s+([A-Za-z0-9_./-]+)\.(version|commit|date)=`)

// TestEveryVersionStamp_TargetsThisPackage is the guard, and it SEARCHES rather
// than enumerating.
//
// An earlier version of this test checked two known files, the Makefile and
// .goreleaser.yaml. A third one existed — scripts/brew-install-no-libkrun.sh
// builds a binary whose version the Homebrew formula's `test do` block asserts
// — and moving this package broke it while the test stayed green. CI caught it
// only because that script happens to run in ci.yml:
//
//	Expected /0\.0\.0\-mgit75check/ to match
//	  "mgit version v0.0.0-20260811034626-f3d026f4bfb3 (commit: ...)"
//
// A list of places to check is itself a thing that drifts. Finding every stamp
// and asserting each one is correct cannot miss a new file. Refs: MGIT-83, MGIT-84
func TestEveryVersionStamp_TargetsThisPackage(t *testing.T) {
	root := repoRoot(t)
	found := 0

	for _, path := range stampCandidates(t, root) {
		body := readFile(t, path)
		for _, m := range stampRe.FindAllStringSubmatch(body, -1) {
			found++
			if m[1] != LDFlagPath {
				rel := mustRel(t, root, path)
				t.Errorf("%s stamps %s.%s, want %s.%s — the linker silently ignores a "+
					"-X for a symbol that does not exist, so this binary ships reporting "+
					"the dev defaults", rel, m[1], m[2], LDFlagPath, m[2])
			}
		}
	}
	if found == 0 {
		t.Fatal("no -X version stamps found anywhere; either the search is broken " +
			"or nothing stamps the build, and every binary reports dev/none/unknown")
	}
	t.Logf("checked %d literal version stamps, all targeting %s", found, LDFlagPath)

	// The Makefile stamps through a variable (`-X $(BUILDINFO).version=`), which
	// the regex above cannot resolve — so it contributes zero matches and would
	// be silently unchecked. Assert its definition directly rather than leaving
	// a blind spot the log's count makes look covered.
	makefile := readFile(t, filepath.Join(root, "Makefile"))
	if !strings.Contains(makefile, "BUILDINFO := "+LDFlagPath) {
		t.Errorf("the Makefile does not define BUILDINFO as %q, so `make build` "+
			"stamps a symbol that does not exist and the binary reports the dev "+
			"defaults", LDFlagPath)
	}
}

// TestGoreleaser_StampsEveryShippedBinary is a separate property from the one
// above: not "is each stamp correct" but "is each SHIPPED binary stamped".
// mgit-sandboxd's builds carried only -s -w until MGIT-83, so it would have
// reported "dev" in every release even with a correct --version flag.
func TestGoreleaser_StampsEveryShippedBinary(t *testing.T) {
	body := readFile(t, filepath.Join(repoRoot(t), ".goreleaser.yaml"))
	for _, field := range []string{"version", "commit", "date"} {
		want := fmt.Sprintf("-X %s.%s=", LDFlagPath, field)
		if got := strings.Count(body, want); got != 3 {
			t.Errorf("goreleaser stamps %s on %d builds, want 3 (mgit, "+
				"mgit-sandboxd-linux, mgit-sandboxd-darwin) — an unstamped build "+
				"ships a binary that cannot say which release it is", want, got)
		}
	}
}

// TestLDFlagPath_IsThisPackagesRealImportPath keeps the constant honest: it is
// only useful as a guard if it names the package it actually lives in.
func TestLDFlagPath_IsThisPackagesRealImportPath(t *testing.T) {
	dir := filepath.Dir(thisFile(t))
	want := "github.com/hyper-swe/mgit/" + filepath.ToSlash(mustRel(t, repoRoot(t), dir))
	if LDFlagPath != want {
		t.Errorf("LDFlagPath = %q, but this package lives at %q; every build config "+
			"is being pointed at a symbol that does not exist", LDFlagPath, want)
	}
}

// stampCandidates lists the files a -X could plausibly live in: the build
// configs and every script. Deliberately broader than the places that stamp
// today — the point is to catch a new one.
func stampCandidates(t *testing.T, root string) []string {
	t.Helper()
	paths := []string{
		filepath.Join(root, "Makefile"),
		filepath.Join(root, ".goreleaser.yaml"),
	}
	for _, dir := range []string{"scripts", ".github/workflows"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() {
				paths = append(paths, p)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}
	return paths
}

func thisFile(t *testing.T) string {
	t.Helper()
	_, f, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this test's own source")
	}
	return f
}

func repoRoot(t *testing.T) string {
	t.Helper()
	// internal/buildinfo/<file> -> module root
	return filepath.Clean(filepath.Join(filepath.Dir(thisFile(t)), "..", ".."))
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
