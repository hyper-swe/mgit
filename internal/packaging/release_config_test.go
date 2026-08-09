// Package packaging holds regression guards for how mgit is DISTRIBUTED.
//
// These are structural tests over the release configuration, not over Go
// code: they exist because the containment pillar (mgit-sandboxd) was built
// on main but never shipped by any channel — the .goreleaser.yaml built only
// cmd/mgit, so brew / go install / release-archive users never got the
// daemon (MGIT-44, same failure class as MGIT-36). A config regression that
// drops mgit-sandboxd from the pipeline is silent and catastrophic for the
// product, so it gets a test that fails loudly.
//
// Refs: MGIT-44, FR-17.16
package packaging

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// repoRoot walks up from the test's working directory until it finds the
// module's go.mod, returning the repository root. It fails the test rather
// than guess, so a moved test file surfaces immediately.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found walking up from test dir")
		}
		dir = parent
	}
}

// readRepoFile reads a repo-relative file, failing the test if it is absent.
func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	//nolint:gosec // G304: test-only; rel is a hardcoded repo-relative path at every call site, joined onto the discovered module root
	b, err := os.ReadFile(filepath.Join(repoRoot(t), rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// TestGoreleaser_BuildsBothHostBinaries proves the release pipeline builds
// the sandbox daemon for every host that has a backend: linux natively
// (CGO-free firecracker) and darwin as a CGO+entitlement prebuilt. This is
// the core MGIT-44 regression guard.
func TestGoreleaser_BuildsBothHostBinaries(t *testing.T) {
	cfg := readRepoFile(t, ".goreleaser.yaml")

	tests := []struct {
		name  string
		token string
	}{
		{"mgit host binary still built", "main: ./cmd/mgit/"},
		{"sandbox daemon is a build target", "main: ./cmd/mgit-sandboxd/"},
		{"linux sandboxd build id", "id: mgit-sandboxd-linux"},
		{"darwin sandboxd build id", "id: mgit-sandboxd-darwin"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !strings.Contains(cfg, tt.token) {
				t.Errorf("`.goreleaser.yaml` missing %q — %s", tt.token, tt.name)
			}
		})
	}
}

// TestGoreleaser_DarwinSandboxdIsCGOAndSigned proves the darwin daemon is
// built with CGO (Virtualization.framework) and entitlement-signed inline via
// a post-build codesign hook — the OSS-goreleaser way to ship a signed CGO
// binary in the same run. Without the entitlement the daemon cannot start a
// VM, so the macOS sandbox would be dead on arrival. Refs: MGIT-44, FR-17.15
func TestGoreleaser_DarwinSandboxdIsCGOAndSigned(t *testing.T) {
	cfg := readRepoFile(t, ".goreleaser.yaml")
	tests := []struct {
		name  string
		token string
	}{
		{"darwin daemon built with CGO", "CGO_ENABLED=1"},
		{"post-build codesign hook", "codesign"},
		{"signs with the virtualization entitlement", "build/darwin/vz.entitlements"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !strings.Contains(cfg, tt.token) {
				t.Errorf("`.goreleaser.yaml` missing %q — %s", tt.token, tt.name)
			}
		})
	}
}

// TestGoreleaser_LinuxSandboxdIsCGOFree guards the invariant that the Linux
// daemon cross-builds without CGO (firecracker is a subprocess; the vzf
// backend compiles to its unavailable stub off darwin). If someone flips it
// to CGO_ENABLED=1 the ubuntu release runner can no longer produce it.
func TestGoreleaser_LinuxSandboxdIsCGOFree(t *testing.T) {
	cfg := readRepoFile(t, ".goreleaser.yaml")
	// The linux sandboxd build block must carry CGO_ENABLED=0. We assert the
	// build id and a CGO_ENABLED=0 env both appear; the block ordering keeps
	// them together (see the config).
	if !strings.Contains(cfg, "id: mgit-sandboxd-linux") {
		t.Fatal("linux sandboxd build id absent")
	}
	if !strings.Contains(cfg, "CGO_ENABLED=0") {
		t.Error("linux sandboxd must build with CGO_ENABLED=0 (pure-Go firecracker path)")
	}
}

// TestGoreleaser_ArchivesShipSandboxd proves both sandboxd builds are folded
// into the release archives, so a downloaded archive actually contains the
// daemon next to mgit (mgit resolves it alongside its own binary).
func TestGoreleaser_ArchivesShipSandboxd(t *testing.T) {
	cfg := readRepoFile(t, ".goreleaser.yaml")
	// Split at the archives: key so we only assert membership within the
	// archives section, not the builds section.
	_, archives, ok := strings.Cut(cfg, "\narchives:")
	if !ok {
		t.Fatal(".goreleaser.yaml has no archives section")
	}
	for _, id := range []string{"mgit-sandboxd-linux", "mgit-sandboxd-darwin"} {
		if !strings.Contains(archives, "- "+id) {
			t.Errorf("archives section does not include build id %q", id)
		}
	}
}

// TestGoreleaser_GuestBinariesShipBesideMgitNotOnHostPath enforces the
// distribution decision as ADR-010 leaves it.
//
// mgit-guest is PID 1 INSIDE the guest and is meaningless on a host. It used
// to reach the guest inside the rootfs image we built and published; under
// libkrun there is no rootfs to publish — the user brings an OCI image and we
// compose a base from it — so the guest binaries have to travel in the mgit
// archive instead. That makes two things load-bearing at once: they must BE
// in the archive (or `brew install mgit` cannot compose a base at all, having
// neither a Go toolchain nor the source), and they must NOT sit next to mgit
// where a package manager would link them onto PATH. A guest/ subdirectory
// satisfies both. Refs: MGIT-44, MGIT-61.15, ADR-005, ADR-010
func TestGoreleaser_GuestBinariesShipBesideMgitNotOnHostPath(t *testing.T) {
	cfg := readRepoFile(t, ".goreleaser.yaml")
	if !strings.Contains(cfg, "main: ./cmd/mgit-guest/") {
		t.Error("mgit-guest must be built for the release: a host install carries no " +
			"Go toolchain and no source, so without it `mgit sandbox base from` cannot compose a base")
	}

	_, archives, ok := strings.Cut(cfg, "\narchives:")
	if !ok {
		t.Fatal(".goreleaser.yaml has no archives section")
	}
	if strings.Contains(archives, "- mgit-guest\n") {
		t.Error("mgit-guest must not be an archive build id — that lands it beside mgit, " +
			"where a package manager links it onto host PATH")
	}
	// Exactly the string the host-side lookup joins onto its own directory.
	// goreleaser templates `src` in this section but NOT `dst`, so this must
	// stay a literal — a `{{ .Arch }}` here would ship as those characters.
	if !strings.Contains(archives, `dst: "guest/"`) {
		t.Error("the guest binaries must be archived under guest/, which is " +
			"where `mgit sandbox base` looks for them")
	}
}

// TestVZEntitlements_PresentAndCorrect guards the entitlement plist the mac
// release job signs the daemon with. Without com.apple.security.virtualization
// the shipped mgit-sandboxd cannot create a VM, so the macOS sandbox is dead
// on arrival. Refs: MGIT-44, FR-17.15
func TestVZEntitlements_PresentAndCorrect(t *testing.T) {
	ent := readRepoFile(t, "build/darwin/vz.entitlements")
	if !strings.Contains(ent, "com.apple.security.virtualization") {
		t.Error("vz.entitlements must grant com.apple.security.virtualization")
	}
	if !strings.Contains(ent, "<plist") {
		t.Error("vz.entitlements must be a plist")
	}
}

// TestReleaseWorkflow_RunsGoreleaserOnMac proves the release job runs on an
// Apple Silicon runner (so goreleaser can natively CGO-build + sign the darwin
// daemon while cross-building the rest) and actually invokes goreleaser.
// Refs: MGIT-44
func TestReleaseWorkflow_RunsGoreleaserOnMac(t *testing.T) {
	wf := readRepoFile(t, ".github/workflows/release.yml")
	tests := []struct {
		name  string
		token string
	}{
		{"release runs on a macOS runner", "runs-on: macos"},
		{"invokes goreleaser", "goreleaser-action"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !strings.Contains(wf, tt.token) {
				t.Errorf("release.yml missing %q — %s", tt.token, tt.name)
			}
		})
	}
}

// TestInstallDoc_CoversGoInstallAndTheGuestBase guards the distribution facts
// the install reference owns: how the daemon is installed, how the guest base
// is provisioned, and where the guest binaries live. The README narrative
// (MGIT-49) links this reference; the facts live here so they cannot silently
// drift from what the commands actually do. Refs: MGIT-44, MGIT-61.15
func TestInstallDoc_CoversGoInstallAndTheGuestBase(t *testing.T) {
	doc := readRepoFile(t, "docs/INSTALL-SANDBOX.md")
	tests := []struct {
		name  string
		token string
	}{
		{"go-install path for the daemon", "go install github.com/hyper-swe/mgit/cmd/mgit-sandboxd@latest"},
		{"the one command that provisions a base", "mgit sandbox base from"},
		{"no default base ships, so launch fails closed", "ships no default base"},
		{"the guest binaries travel in the archive", "guest/"},
		{"guest binary is not on host PATH", "mgit-guest"},
		{"the kernel+rootfs path still has its build ticket", "MGIT-30"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !strings.Contains(doc, tt.token) {
				t.Errorf("docs/INSTALL-SANDBOX.md missing %q — %s", tt.token, tt.name)
			}
		})
	}
}

// TestGoreleaserConfig_IsValid runs `goreleaser check` when the binary is
// available, catching schema errors my edits might introduce. It skips
// (does not fail) where goreleaser is absent so the unit suite stays
// hermetic; the release preflight runs the real thing.
func TestGoreleaserConfig_IsValid(t *testing.T) {
	bin, err := exec.LookPath("goreleaser")
	if err != nil {
		t.Skip("goreleaser not on PATH; skipping schema validation (release preflight covers it)")
	}
	//nolint:gosec // G204: bin is resolved from PATH via LookPath and the args are fixed literals; no user input
	cmd := exec.Command(bin, "check")
	cmd.Dir = repoRoot(t)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("goreleaser check failed: %v\n%s", err, out)
	}
}

// TestBrewFormula_InstallsTheGuestBinaries closes the install path most macOS
// users take, and the one that reproduces MGIT-65's second defect most
// exactly: the formula installed `mgit` and `mgit-sandboxd` and nothing from
// the archive's `guest/` directory, so a brewed mgit resolved no guest
// binaries, fell through to the source build, and died with "cannot find main
// module" on a machine that has never had the mgit source.
//
// libexec, not bin: everything Homebrew puts in bin is linked onto PATH, and
// mgit-guest on a host PATH is exactly what the distribution boundary forbids.
// Refs: MGIT-65, MGIT-44, MGIT-61.15
func TestBrewFormula_InstallsTheGuestBinaries(t *testing.T) {
	formula := readRepoFile(t, "brew/mgit.rb")

	if !strings.Contains(formula, `libexec.install "guest"`) {
		t.Error("the formula must install the archive's guest/ directory into libexec; " +
			"without it a brewed mgit cannot compose a guest base at all")
	}
	if strings.Contains(formula, `bin.install "guest"`) {
		t.Error("guest binaries must not land in bin: Homebrew links bin onto PATH, " +
			"and mgit-guest is guest-only")
	}
}

// thirdPartyDependsOnRe matches a `depends_on` whose formula name is
// tap-qualified (`user/tap/formula`) — i.e. any dependency Homebrew has to
// reach outside homebrew/core to resolve. Homebrew/core dependencies are bare
// names and stay legal.
var thirdPartyDependsOnRe = regexp.MustCompile(`depends_on\s+"[^"/]+/[^"/]+/[^"]+"`)

// findThirdPartyDependsOn returns the first tap-qualified `depends_on` in
// Ruby CODE, or "" when there is none. Comment lines are skipped: the formula
// documents the removed dependency verbatim so it is not reintroduced by
// someone who does not know why it went, and a check that cannot tell a
// warning from the thing it warns about is useless.
func findThirdPartyDependsOn(formula string) string {
	for _, line := range strings.Split(formula, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if got := thirdPartyDependsOnRe.FindString(line); got != "" {
			return got
		}
	}
	return ""
}

// TestBrewFormula_DeclaresNoThirdPartyTapDependency is the MGIT-75 regression
// guard: `brew install hyper-swe/tap/mgit` must not require a second,
// untrusted tap.
//
// The formula declared `depends_on "libkrun/krun/libkrun"`, and Homebrew
// refuses to LOAD a formula from an untrusted third-party tap. Resolving that
// dependency therefore aborted the install before anything was downloaded —
// exit 1, nothing installed — for every user who did not already have libkrun.
// Reproduced on a Homebrew prefix with libkrun genuinely absent, in both
// states a real user can be in: with the libkrun/krun tap absent ("No
// available formula ... This command requires the tap libkrun/krun") and with
// it tapped but not trusted ("Refusing to load formula libkrun/krun/libkrun
// from untrusted tap").
//
// `=> :optional` is NOT an acceptable fix and must not be reintroduced. It
// does dodge the load (measured), but its opt-in path (`--with-libkrun`)
// still dies on libkrunfw — libkrun's own dependency from the same untrusted
// tap, which no ARGV entry can whitelist. That would advertise an activation
// route that does not work, which is worse than not declaring one.
//
// Core mgit never links libkrun (CGO-free); only mgit-sandboxd does, and it
// fails closed with an actionable message when the library is missing
// (missingLibraryRemedy). So the dependency is genuinely optional at runtime
// and belongs in the caveats, not in the dependency graph. Refs: MGIT-75
func TestBrewFormula_DeclaresNoThirdPartyTapDependency(t *testing.T) {
	formula := readRepoFile(t, "brew/mgit.rb")

	if got := findThirdPartyDependsOn(formula); got != "" {
		t.Errorf("the formula declares a third-party-tap dependency (%s); Homebrew cannot "+
			"resolve one from an untrusted tap, so this breaks `brew install "+
			"hyper-swe/tap/mgit` for every user who does not already have it installed. "+
			"Document the activation step in caveats instead", got)
	}
}

// TestFindThirdPartyDependsOn is the test for the detector above. The guard
// it powers is only worth having if it still fires on the exact line that
// caused MGIT-75, so that case is checked directly rather than assumed —
// the comment-skipping added to stop it matching the formula's own warning
// is precisely the kind of change that can blind it.
func TestFindThirdPartyDependsOn(t *testing.T) {
	tests := []struct {
		name    string
		formula string
		want    string
	}{
		{
			name:    "the_mgit_75_regression",
			formula: "on_macos do\n  depends_on \"libkrun/krun/libkrun\"\nend\n",
			want:    `depends_on "libkrun/krun/libkrun"`,
		},
		{
			name:    "optional_tag_is_still_third_party",
			formula: "  depends_on \"libkrun/krun/libkrun\" => :optional\n",
			want:    `depends_on "libkrun/krun/libkrun"`,
		},
		{
			name:    "commented_out_is_not_a_dependency",
			formula: "  # depends_on \"libkrun/krun/libkrun\"  <- do not add this back\n",
			want:    "",
		},
		{
			name:    "homebrew_core_dependency_is_fine",
			formula: "  depends_on \"openssl@3\"\n",
			want:    "",
		},
		{
			name:    "no_dependencies_at_all",
			formula: "class Mgit < Formula\nend\n",
			want:    "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := findThirdPartyDependsOn(tt.formula); got != tt.want {
				t.Errorf("findThirdPartyDependsOn() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestBrewFormula_CaveatsGiveAWorkingSandboxActivation checks the other half
// of the MGIT-75 fix: dropping the dependency is only correct if the sandbox
// stays discoverable, and the activation it points at has to be a sequence
// that actually runs. `brew install libkrun` on its own does not — the tap
// must be trusted first. Refs: MGIT-75
func TestBrewFormula_CaveatsGiveAWorkingSandboxActivation(t *testing.T) {
	formula := readRepoFile(t, "brew/mgit.rb")

	trustAt := strings.Index(formula, "brew trust libkrun/krun")
	if trustAt == -1 {
		t.Fatal("the caveats must name `brew trust libkrun/krun`: without it the libkrun " +
			"install they suggest fails on Homebrew's untrusted-tap gate")
	}
	installAt := strings.Index(formula, "brew install libkrun")
	if installAt == -1 {
		t.Fatal("the caveats must still name the libkrun install command")
	}
	if trustAt > installAt {
		t.Error("the caveats must tell the user to trust the tap BEFORE installing libkrun; " +
			"the reverse order is the failure this task exists to fix")
	}
}

// TestCIWorkflow_ProvesTheBrewInstallOnALibkrunFreeRunner is the guard on the
// guard. MGIT-75 was invisible to every check we had because every machine
// that runs them already had libkrun — a dry-run on such a machine never has
// to load the dependency formula at all, so it passes.
//
// The job asserted here must therefore do two things, and asserting only the
// first would recreate the false green: run the install on a runner where
// libkrun is absent, and PROVE that absence before drawing any conclusion
// from the result. Refs: MGIT-75, MGIT-69, MGIT-70
func TestCIWorkflow_ProvesTheBrewInstallOnALibkrunFreeRunner(t *testing.T) {
	ci := readRepoFile(t, ".github/workflows/ci.yml")

	job, ok := cutSection(ci, "  brew-install-no-libkrun:", "\n  # ")
	if !ok {
		t.Fatal("CI must have a `brew-install-no-libkrun` job; without one, dropping the " +
			"libkrun dependency can be silently reverted and nothing will notice")
	}
	if !strings.Contains(job, "runs-on: macos") {
		t.Error("the job must run on macOS: the dependency that broke was macOS-only")
	}
	if !strings.Contains(job, checkScript) {
		t.Errorf("the job must run %s", checkScript)
	}
	// The job must not provision libkrun itself. If it did, the dependency
	// would resolve, the install would pass, and the job would certify the
	// exact breakage it exists to catch.
	if strings.Contains(job, "brew install libkrun") {
		t.Error("the job must NOT install libkrun; a satisfied dependency is never " +
			"loaded, which is the whole reason this failure went unnoticed")
	}

	script := readRepoFile(t, checkScript)
	// The precondition. A check that merely installs and passes proves nothing
	// unless it first established that libkrun was not already there.
	if !strings.Contains(script, "brew list --versions libkrun") {
		t.Error("the check must prove libkrun is absent before it trusts its own result; " +
			"a pass on a machine that already has libkrun is exactly the false green " +
			"it exists to prevent")
	}
	if !strings.Contains(script, `"$BREW" install "$FORMULA"`) {
		t.Error("the check must run a REAL brew install of the formula")
	}
	// --dry-run is what passed while the bug was live: it never has to fetch,
	// so on a machine with the dependency satisfied it resolves nothing. The
	// script explains that in prose, so only executable lines are checked.
	if line, found := findInCode(script, "--dry-run"); found {
		t.Errorf("the check must not use `brew install --dry-run` (%q); that is the "+
			"command that reported success while the install was broken", line)
	}
}

// findInCode reports whether `needle` appears on a line of a shell script that
// is not a comment, returning that line. A guard that cannot distinguish a
// command from a comment warning against it is not a guard.
func findInCode(script, needle string) (string, bool) {
	for _, line := range strings.Split(script, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.Contains(trimmed, needle) {
			return trimmed, true
		}
	}
	return "", false
}

// checkScript is the repo-relative path of the first-hour-install check. CI
// and a developer reproducing locally run the same file, so neither can go
// green in a way the other would not.
const checkScript = "scripts/brew-install-no-libkrun.sh"

// cutSection returns the text from the line beginning `start` up to the next
// occurrence of `end`, which is how these workflow assertions scope
// themselves to one job rather than the whole file.
func cutSection(s, start, end string) (string, bool) {
	i := strings.Index(s, start)
	if i == -1 {
		return "", false
	}
	rest := s[i+len(start):]
	if j := strings.Index(rest, end); j != -1 {
		return rest[:j], true
	}
	return rest, true
}
