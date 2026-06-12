package scaffold

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// readApprovedPackages reads APPROVED-PACKAGES.md from the repo root.
func readApprovedPackages(t *testing.T) string {
	t.Helper()
	root := projectRoot(t)
	data, err := os.ReadFile(filepath.Join(root, "APPROVED-PACKAGES.md")) //nolint:gosec // path is test-derived, not user input
	require.NoError(t, err, "APPROVED-PACKAGES.md must exist")
	return string(data)
}

// sandboxDeps are the backend dependencies proposed for the FR-17
// sandbox per ADR-005 adoption criterion 2. Refs: FR-17.15, FR-17.16,
// MGIT-11.1.4
var sandboxDeps = []struct {
	name    string
	pkg     string
	cgoNote bool // CGO-bearing: must be confined to mgit-sandboxd
}{
	{name: "linux_vmm", pkg: "github.com/firecracker-microvm/firecracker-go-sdk", cgoNote: false},
	{name: "macos_vzf", pkg: "github.com/Code-Hex/vz/v3", cgoNote: true},
	{name: "windows_hyperv", pkg: "github.com/Microsoft/hcsshim", cgoNote: false},
	{name: "vsock", pkg: "github.com/mdlayher/vsock", cgoNote: false},
}

// TestApprovedPackages_SandboxDepsListed verifies every sandbox backend
// dependency is registered in APPROVED-PACKAGES.md, with CGO-bearing
// packages confined to mgit-sandboxd. Refs: FR-17.16, MGIT-11.1.4
func TestApprovedPackages_SandboxDepsListed(t *testing.T) {
	content := readApprovedPackages(t)

	require.Contains(t, content, "mgit-sandboxd",
		"registry must have a sandbox-helper section confining backend deps")

	for _, dep := range sandboxDeps {
		t.Run(dep.name, func(t *testing.T) {
			row := registryRow(content, dep.pkg)
			assert.NotEmpty(t, row,
				"sandbox dependency %s must have a registry table row", dep.pkg)
			if dep.cgoNote {
				assert.Contains(t, row, "CGO",
					"CGO-bearing %s must be marked as CGO-confined", dep.pkg)
			}
		})
	}
}

// semverRe matches a pinned minimum version like 1.2.3 in a registry row.
var semverRe = regexp.MustCompile(`\d+\.\d+\.\d+`)

// TestApprovedPackages_PinnedVersions verifies every sandbox dependency
// row pins a minimum semantic version. Refs: MGIT-11.1.4
func TestApprovedPackages_PinnedVersions(t *testing.T) {
	content := readApprovedPackages(t)

	for _, dep := range sandboxDeps {
		t.Run(dep.name, func(t *testing.T) {
			row := registryRow(content, dep.pkg)
			require.NotEmpty(t, row, "registry row for %s must exist", dep.pkg)
			assert.Regexp(t, semverRe, row,
				"registry row for %s must pin a minimum version", dep.pkg)
		})
	}
}

// TestGoMod_NoUnapprovedDeps verifies every direct dependency in go.mod
// is registered in APPROVED-PACKAGES.md (CLAUDE.md rule 3). Refs:
// MGIT-11.1.4
func TestGoMod_NoUnapprovedDeps(t *testing.T) {
	goMod := readGoMod(t)
	// Only the approved sections count: a module path appearing in the
	// "Explicitly Rejected Packages" table must NOT satisfy this gate.
	approved := approvedSections(readApprovedPackages(t))

	for _, dep := range directRequires(goMod) {
		t.Run(dep, func(t *testing.T) {
			assert.NotEmpty(t, registryRow(approved, dep),
				"direct dependency %s must have an approved-registry table row", dep)
		})
	}
}

// approvedSections returns the registry content preceding the
// "Explicitly Rejected Packages" section.
func approvedSections(content string) string {
	if i := strings.Index(content, "Explicitly Rejected"); i >= 0 {
		return content[:i]
	}
	return content
}

// registryRow returns the first markdown table row whose backticked
// package cell names pkg. Prose mentions of a module path do not match.
func registryRow(content, pkg string) string {
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "|") &&
			strings.Contains(line, "`"+pkg+"`") {
			return line
		}
	}
	return ""
}

// directRequires parses the module paths of all non-indirect require
// directives in a go.mod file.
func directRequires(goMod string) []string {
	var out []string
	inBlock := false
	for _, line := range strings.Split(goMod, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "require (" {
			inBlock = true
			continue
		}
		if inBlock && trimmed == ")" {
			inBlock = false
			continue
		}
		if !inBlock && !strings.HasPrefix(trimmed, "require ") {
			continue
		}
		if strings.Contains(trimmed, "// indirect") {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(trimmed, "require "))
		if len(fields) >= 2 && strings.Contains(fields[0], "/") {
			out = append(out, fields[0])
		}
	}
	return out
}
