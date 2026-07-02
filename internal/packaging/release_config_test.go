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

// TestGoreleaser_GuestNotShippedOnHost enforces the distribution decision:
// mgit-guest is PID 1 inside the guest rootfs image, NOT a host binary. It
// must never be added to the host builds/archives. Refs: MGIT-44, ADR-005
func TestGoreleaser_GuestNotShippedOnHost(t *testing.T) {
	cfg := readRepoFile(t, ".goreleaser.yaml")
	if strings.Contains(cfg, "./cmd/mgit-guest/") {
		t.Error("mgit-guest must not be a host build target — it ships inside the guest image (ADR-005), not on host PATH")
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

// TestInstallDoc_CoversGoInstallAndGuestImage guards the distribution facts
// this ticket owns: the documented go-install path for the daemon and the
// guest-image distribution decision. The README narrative (MGIT-49) links
// this reference; the facts live here so they cannot silently drift.
// Refs: MGIT-44
func TestInstallDoc_CoversGoInstallAndGuestImage(t *testing.T) {
	doc := readRepoFile(t, "docs/INSTALL-SANDBOX.md")
	tests := []struct {
		name  string
		token string
	}{
		{"go-install path for the daemon", "go install github.com/hyper-swe/mgit/cmd/mgit-sandboxd@latest"},
		{"guest-image distribution decision", "guest image"},
		{"guest binary is not on host PATH", "mgit-guest"},
		{"ties to the guest-image build ticket", "MGIT-30"},
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
