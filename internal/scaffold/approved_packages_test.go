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
			assert.Contains(t, content, dep.pkg,
				"sandbox dependency %s must be registered", dep.pkg)
			if dep.cgoNote {
				line := lineContaining(content, dep.pkg)
				assert.Contains(t, line, "CGO",
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
			line := lineContaining(content, dep.pkg)
			require.NotEmpty(t, line, "registry row for %s must exist", dep.pkg)
			assert.Regexp(t, semverRe, line,
				"registry row for %s must pin a minimum version", dep.pkg)
		})
	}
}

// TestGoMod_NoUnapprovedDeps verifies every direct dependency in go.mod
// is registered in APPROVED-PACKAGES.md (CLAUDE.md rule 3). Refs:
// MGIT-11.1.4
func TestGoMod_NoUnapprovedDeps(t *testing.T) {
	goMod := readGoMod(t)
	approved := readApprovedPackages(t)

	for _, dep := range directRequires(goMod) {
		t.Run(dep, func(t *testing.T) {
			assert.Contains(t, approved, dep,
				"direct dependency %s must be registered in APPROVED-PACKAGES.md", dep)
		})
	}
}

// lineContaining returns the first line of content containing substr.
func lineContaining(content, substr string) string {
	for _, line := range strings.Split(content, "\n") {
		if strings.Contains(line, substr) {
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
