package docs

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testRootCmd() *cobra.Command {
	root := &cobra.Command{Use: "mgit", Short: "test mgit"}
	root.AddCommand(
		&cobra.Command{Use: "init", Short: "Initialize repository"},
		&cobra.Command{Use: "commit", Short: "Create commit"},
		&cobra.Command{Use: "log", Short: "Show history"},
	)
	return root
}

func testMCPTools() []MCPToolInfo {
	return []MCPToolInfo{
		{Name: "mgit_commit", Description: "Create commit", Parameters: []string{"task_id", "message"}},
		{Name: "mgit_log", Description: "Show history", Parameters: []string{"task_id"}},
		{Name: "mgit_squash", Description: "Squash commits", Parameters: []string{"task_id"}},
	}
}

func fixedClock() func() time.Time {
	return func() time.Time { return time.Date(2026, 4, 7, 12, 0, 0, 0, time.UTC) }
}

func TestGenerator_ProducesAllFiles(t *testing.T) {
	outDir := filepath.Join(t.TempDir(), "docs")
	gen := NewGenerator(outDir, testRootCmd(), testMCPTools(), "1.0.0", fixedClock())

	results, err := gen.Generate(false)
	require.NoError(t, err)
	assert.Len(t, results, 9, "must produce 9 files")

	for _, r := range results {
		assert.Equal(t, "created", r.Action, "all files should be created on first run")
		assert.FileExists(t, filepath.Join(outDir, r.File))
	}
}

func TestGenerator_AutoGenAlwaysRegenerated(t *testing.T) {
	outDir := filepath.Join(t.TempDir(), "docs")
	gen := NewGenerator(outDir, testRootCmd(), testMCPTools(), "1.0.0", fixedClock())

	// First run
	_, err := gen.Generate(false)
	require.NoError(t, err)

	// Second run (no force)
	results, err := gen.Generate(false)
	require.NoError(t, err)

	autoGenFiles := map[string]bool{
		"CLI_REFERENCE.md": true, "MCP_TOOLS.md": true, "SKILL.md": true,
	}

	for _, r := range results {
		if autoGenFiles[r.File] {
			assert.Equal(t, "updated", r.Action,
				"auto-gen file %s must be regenerated", r.File)
		}
	}
}

func TestGenerator_TemplateSkippedIfExists(t *testing.T) {
	outDir := filepath.Join(t.TempDir(), "docs")
	gen := NewGenerator(outDir, testRootCmd(), testMCPTools(), "1.0.0", fixedClock())

	// First run
	_, err := gen.Generate(false)
	require.NoError(t, err)

	// Second run (no force)
	results, err := gen.Generate(false)
	require.NoError(t, err)

	templateFiles := map[string]bool{
		"AGENTS.md": true, "CLAUDE.md": true, "WORKFLOWS.md": true,
		"ROLLBACK_GUIDE.md": true, "SQUASH_GUIDE.md": true, "TROUBLESHOOTING.md": true,
	}

	for _, r := range results {
		if templateFiles[r.File] {
			assert.Equal(t, "skipped", r.Action,
				"template file %s must be skipped if exists", r.File)
		}
	}
}

func TestGenerator_ForceRegeneratesTemplates(t *testing.T) {
	outDir := filepath.Join(t.TempDir(), "docs")
	gen := NewGenerator(outDir, testRootCmd(), testMCPTools(), "1.0.0", fixedClock())

	// First run
	_, err := gen.Generate(false)
	require.NoError(t, err)

	// Force run
	results, err := gen.Generate(true)
	require.NoError(t, err)

	for _, r := range results {
		assert.Equal(t, "updated", r.Action,
			"force must regenerate all files including %s", r.File)
	}
}

func TestGenerator_OutputDirectoryCreated(t *testing.T) {
	outDir := filepath.Join(t.TempDir(), "new", "nested", "docs")
	gen := NewGenerator(outDir, testRootCmd(), testMCPTools(), "1.0.0", fixedClock())

	_, err := gen.Generate(false)
	require.NoError(t, err)
	assert.DirExists(t, outDir)
}

func TestGenerator_CLIReference_ContainsCommands(t *testing.T) {
	outDir := filepath.Join(t.TempDir(), "docs")
	gen := NewGenerator(outDir, testRootCmd(), testMCPTools(), "1.0.0", fixedClock())

	_, err := gen.Generate(false)
	require.NoError(t, err)

	data, err := os.ReadFile(filepath.Join(outDir, "CLI_REFERENCE.md")) //nolint:gosec // test path
	require.NoError(t, err)
	content := string(data)

	assert.Contains(t, content, "mgit init")
	assert.Contains(t, content, "mgit commit")
	assert.Contains(t, content, "mgit log")
	assert.Contains(t, content, "AUTO-GENERATED")
}

func TestGenerator_MCPTools_ContainsTools(t *testing.T) {
	outDir := filepath.Join(t.TempDir(), "docs")
	gen := NewGenerator(outDir, testRootCmd(), testMCPTools(), "1.0.0", fixedClock())

	_, err := gen.Generate(false)
	require.NoError(t, err)

	data, err := os.ReadFile(filepath.Join(outDir, "MCP_TOOLS.md")) //nolint:gosec // test path
	require.NoError(t, err)
	content := string(data)

	assert.Contains(t, content, "mgit_commit")
	assert.Contains(t, content, "mgit_log")
	assert.Contains(t, content, "**Total tools:** 3")
}

func TestGenerator_Skill_HasYAMLFrontmatter(t *testing.T) {
	outDir := filepath.Join(t.TempDir(), "docs")
	gen := NewGenerator(outDir, testRootCmd(), testMCPTools(), "1.0.0", fixedClock())

	_, err := gen.Generate(false)
	require.NoError(t, err)

	data, err := os.ReadFile(filepath.Join(outDir, "SKILL.md")) //nolint:gosec // test path
	require.NoError(t, err)
	content := string(data)

	assert.True(t, content[:4] == "---\n", "SKILL.md must start with YAML frontmatter")
	assert.Contains(t, content, "name: mgit")
	assert.Contains(t, content, "version: 1.0.0")
	assert.Contains(t, content, "mgit_commit")
}
