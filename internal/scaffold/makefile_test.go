// Package scaffold validates project scaffolding correctness.
// These tests verify the Makefile per MGIT-1.2.3 acceptance criteria.
// Refs: NFR-4
package scaffold

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// readMakefile reads and returns the Makefile contents.
func readMakefile(t *testing.T) string {
	t.Helper()
	root := projectRoot(t)
	data, err := os.ReadFile(filepath.Join(root, "Makefile")) //nolint:gosec // path is test-derived, not user input
	require.NoError(t, err, "failed to read Makefile")
	return string(data)
}

func TestMakefile_Exists(t *testing.T) {
	root := projectRoot(t)
	_, err := os.Stat(filepath.Join(root, "Makefile"))
	assert.NoError(t, err, "Makefile must exist at repository root")
}

func TestMakefile_AllRequiredTargets(t *testing.T) {
	content := readMakefile(t)

	requiredTargets := []string{
		"build",
		"test",
		"test-race",
		"test-cover",
		"lint",
		"security-scan",
		"bench",
		"clean",
	}

	for _, target := range requiredTargets {
		t.Run(target, func(t *testing.T) {
			// Target should appear as a rule (target: ...)
			assert.Contains(t, content, target+":",
				"Makefile must contain target: %s", target)
		})
	}
}

func TestMakefile_PhonyTargets(t *testing.T) {
	content := readMakefile(t)

	// All targets must be marked as .PHONY
	requiredTargets := []string{
		"build", "test", "test-race", "test-cover",
		"lint", "security-scan", "bench", "clean",
	}

	for _, target := range requiredTargets {
		t.Run(target, func(t *testing.T) {
			// Check that target appears in a .PHONY declaration
			phonyFound := false
			for _, line := range strings.Split(content, "\n") {
				trimmed := strings.TrimSpace(line)
				if strings.HasPrefix(trimmed, ".PHONY:") && strings.Contains(trimmed, target) {
					phonyFound = true
					break
				}
			}
			assert.True(t, phonyFound,
				"target %s must be listed in .PHONY", target)
		})
	}
}

func TestMakefile_BuildTarget(t *testing.T) {
	content := readMakefile(t)
	// build target must reference cmd/mgit
	assert.Contains(t, content, "cmd/mgit",
		"build target must compile cmd/mgit")
}

func TestMakefile_TestTarget(t *testing.T) {
	content := readMakefile(t)
	assert.Contains(t, content, "go test ./...",
		"test target must run go test ./...")
}

func TestMakefile_TestRaceTarget(t *testing.T) {
	content := readMakefile(t)
	assert.Contains(t, content, "-race",
		"test-race target must use -race flag")
}

func TestMakefile_LintTarget(t *testing.T) {
	content := readMakefile(t)
	assert.Contains(t, content, "golangci-lint",
		"lint target must invoke golangci-lint")
}

func TestMakefile_SecurityScanTarget(t *testing.T) {
	content := readMakefile(t)
	assert.Contains(t, content, "govulncheck",
		"security-scan target must invoke govulncheck")
}

func TestMakefile_DefaultGoal(t *testing.T) {
	content := readMakefile(t)
	assert.Contains(t, content, ".DEFAULT_GOAL",
		"Makefile must set .DEFAULT_GOAL")
	assert.Contains(t, content, "test",
		"DEFAULT_GOAL should be test")
}
