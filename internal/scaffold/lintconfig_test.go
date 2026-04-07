// Package scaffold validates project scaffolding correctness.
// These tests verify the golangci-lint configuration per MGIT-1.2.4.
// Refs: NFR-4, QUALITY-STANDARDS.md Section 4.1
package scaffold

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// readLintConfig reads and returns the golangci-lint config file contents.
func readLintConfig(t *testing.T) string {
	t.Helper()
	root := projectRoot(t)
	data, err := os.ReadFile(filepath.Join(root, ".golangci.yml")) //nolint:gosec // path is test-derived, not user input
	require.NoError(t, err, "failed to read .golangci.yml")
	return string(data)
}

func TestGolangciConfig_Exists(t *testing.T) {
	root := projectRoot(t)
	_, err := os.Stat(filepath.Join(root, ".golangci.yml"))
	assert.NoError(t, err, ".golangci.yml must exist at repository root")
}

func TestGolangciConfig_ValidYAML(t *testing.T) {
	content := readLintConfig(t)
	// Basic YAML validity: must not be empty and must contain key structural elements
	assert.NotEmpty(t, content, ".golangci.yml must not be empty")
	assert.Contains(t, content, "linters",
		".golangci.yml must contain linters configuration")
}

func TestGolangciConfig_AllLintersEnabled(t *testing.T) {
	content := readLintConfig(t)

	// golangci-lint v2 uses linters.enable list
	// Must enable at least 15 linters per QUALITY-STANDARDS.md
	requiredLinters := []string{
		"gosec",
		"staticcheck",
		"govet",
		"errcheck",
		"ineffassign",
		"unconvert",
		"misspell",
		"bodyclose",
		"noctx",
		"sqlclosecheck",
		"gocritic",
		"revive",
		"prealloc",
		"unused",
		"usestdlibvars",
	}

	enabledCount := 0
	for _, linter := range requiredLinters {
		if strings.Contains(content, linter) {
			enabledCount++
		}
	}
	assert.GreaterOrEqual(t, enabledCount, 15,
		"must enable at least 15 linters, found %d", enabledCount)
}

func TestGolangciConfig_ComplexityLimits(t *testing.T) {
	content := readLintConfig(t)

	// Verify complexity settings exist
	assert.Contains(t, content, "max-complexity",
		"config must set max-complexity")
	// The value 10 should appear near max-complexity
	assert.Contains(t, content, "10",
		"max-complexity should be set to 10")
}

func TestGolangciConfig_SecurityLintersEnabled(t *testing.T) {
	content := readLintConfig(t)

	securityLinters := []string{
		"gosec",
		"sqlclosecheck",
	}

	for _, linter := range securityLinters {
		t.Run(linter, func(t *testing.T) {
			assert.Contains(t, content, linter,
				"security linter %s must be enabled", linter)
		})
	}
}
