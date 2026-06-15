// Package scaffold validates project scaffolding correctness.
// These tests verify the Go module is initialized per MGIT-1.2.1 acceptance criteria.
// Refs: FR-1.1, NFR-4, NFR-5
package scaffold

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// projectRoot returns the absolute path to the repository root.
// It walks up from the test file's directory until it finds go.mod.
func projectRoot(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	require.True(t, ok, "failed to get caller info")

	dir := filepath.Dir(filename)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		require.NotEqual(t, dir, parent, "go.mod not found in any parent directory")
		dir = parent
	}
}

// readGoMod reads and returns the go.mod file contents.
func readGoMod(t *testing.T) string {
	t.Helper()
	root := projectRoot(t)
	data, err := os.ReadFile(filepath.Join(root, "go.mod")) //nolint:gosec // path is test-derived, not user input
	require.NoError(t, err, "failed to read go.mod")
	return string(data)
}

func TestGoMod_Exists(t *testing.T) {
	root := projectRoot(t)
	goModPath := filepath.Join(root, "go.mod")
	_, err := os.Stat(goModPath)
	assert.NoError(t, err, "go.mod must exist at repository root")
}

func TestGoMod_CorrectModulePath(t *testing.T) {
	content := readGoMod(t)
	assert.Contains(t, content, "module github.com/hyper-swe/mgit",
		"go.mod must declare module path as github.com/hyper-swe/mgit")
}

func TestGoMod_GoVersionDirective(t *testing.T) {
	content := readGoMod(t)
	// go directive must be >= 1.23
	lines := strings.Split(content, "\n")
	found := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "go ") {
			found = true
			version := strings.TrimPrefix(trimmed, "go ")
			version = strings.TrimSpace(version)
			// Extract major.minor
			parts := strings.Split(version, ".")
			require.GreaterOrEqual(t, len(parts), 2,
				"go directive must have at least major.minor")
			assert.Equal(t, "1", parts[0], "go major version must be 1")
			minor := parts[1]
			// minor must be >= 23
			assert.GreaterOrEqual(t, minor, "23",
				"go minor version must be >= 23, got %s", minor)
			break
		}
	}
	assert.True(t, found, "go.mod must contain a 'go' directive")
}

func TestGoMod_AllApprovedDepsPresent(t *testing.T) {
	content := readGoMod(t)

	// All approved dependencies from APPROVED-PACKAGES.md
	approvedDeps := []struct {
		name       string
		minVersion string
	}{
		{"github.com/go-git/go-git/v5", "v5.13.0"},
		{"modernc.org/sqlite", "v1.35.0"},
		{"github.com/spf13/cobra", "v1.8.1"},
		{"github.com/labstack/echo/v4", "v4.13.4"},
		{"github.com/stretchr/testify", "v1.10.0"},
		{"github.com/oklog/ulid/v2", "v2.1.0"},
		{name: "golang.org/x/sync"},
		{name: "golang.org/x/crypto"},
	}

	for _, dep := range approvedDeps {
		t.Run(dep.name, func(t *testing.T) {
			assert.Contains(t, content, dep.name,
				"go.mod must contain approved dependency: %s", dep.name)
		})
	}
}

func TestGoMod_NoUnauthorizedDeps(t *testing.T) {
	content := readGoMod(t)

	// Explicitly forbidden packages per APPROVED-PACKAGES.md §4.
	// logrus and pkg/errors are deliberately NOT listed here: they enter
	// the graph only as dependencies of the ADR-005-approved
	// firecracker-go-sdk (§2a) — logrus as its required logging adapter,
	// pkg/errors as a pure transitive. A whole-go.mod string scan cannot
	// tell "core adopted a rejected package" from "an approved sandbox
	// SDK depends on one". That distinction is enforced precisely by
	// TestImports_SandboxDepsConfinedToSandboxd, which forbids both from
	// every core import graph. The packages below are only ever DIRECT
	// choices, so a go.mod scan remains the right guard for them.
	forbiddenDeps := []string{
		"github.com/mattn/go-sqlite3",
		"gorm.io/gorm",
		"github.com/jmoiron/sqlx",
		"github.com/libgit2/git2go",
		"github.com/gin-gonic/gin",
		"github.com/spf13/viper",
	}

	for _, dep := range forbiddenDeps {
		t.Run(dep, func(t *testing.T) {
			assert.NotContains(t, content, dep,
				"go.mod must NOT contain forbidden dependency: %s", dep)
		})
	}
}

func TestGoMod_TidySucceeds(t *testing.T) {
	root := projectRoot(t)
	ctx := context.Background()
	cmd := exec.CommandContext(ctx, "go", "mod", "tidy")
	cmd.Dir = root
	output, err := cmd.CombinedOutput()
	assert.NoError(t, err,
		"go mod tidy must succeed without errors; output: %s", string(output))
}

func TestGoMod_CleanDependencyTree(t *testing.T) {
	root := projectRoot(t)
	ctx := context.Background()
	cmd := exec.CommandContext(ctx, "go", "list", "-m", "all")
	cmd.Dir = root
	output, err := cmd.CombinedOutput()
	require.NoError(t, err,
		"go list -m all must succeed; output: %s", string(output))

	// Verify the module itself is listed
	assert.Contains(t, string(output), "github.com/hyper-swe/mgit",
		"dependency tree must include the module itself")
}
