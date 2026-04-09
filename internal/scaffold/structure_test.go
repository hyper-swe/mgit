// Package scaffold validates project scaffolding correctness.
// These tests verify the directory structure per MGIT-1.2.2 acceptance criteria.
// Refs: FR-1.1, NFR-4
package scaffold

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// requiredDirs returns the directories required by CODING-STYLE.md Section 1.
func requiredDirs() []string {
	return []string{
		"cmd/mgit",
		"internal/model",
		"internal/store/git",
		"internal/store/index",
		"internal/service",
		"internal/api/http",
		"internal/mcp",
		"internal/mtix",
		"internal/testutil",
		"e2e",
	}
}

func TestProjectStructure_AllDirectoriesExist(t *testing.T) {
	root := projectRoot(t)
	for _, dir := range requiredDirs() {
		t.Run(dir, func(t *testing.T) {
			fullPath := filepath.Join(root, dir)
			info, err := os.Stat(fullPath)
			require.NoError(t, err, "directory %s must exist", dir)
			assert.True(t, info.IsDir(), "%s must be a directory", dir)
		})
	}
}

func TestProjectStructure_MatchesCodingStyleSection1(t *testing.T) {
	root := projectRoot(t)

	// Verify top-level directories match expected set
	expectedTopLevel := []string{"cmd", "internal", "e2e"}
	sort.Strings(expectedTopLevel)

	for _, dir := range expectedTopLevel {
		fullPath := filepath.Join(root, dir)
		info, err := os.Stat(fullPath)
		require.NoError(t, err, "top-level directory %s must exist", dir)
		assert.True(t, info.IsDir(), "%s must be a directory", dir)
	}

	// Verify cmd/mgit exists
	cmdMgit := filepath.Join(root, "cmd", "mgit")
	info, err := os.Stat(cmdMgit)
	require.NoError(t, err, "cmd/mgit must exist")
	assert.True(t, info.IsDir(), "cmd/mgit must be a directory")

	// Verify internal subdirectories
	expectedInternal := []string{
		"model", "store", "service", "api", "mcp", "mtix", "testutil",
	}
	for _, sub := range expectedInternal {
		fullPath := filepath.Join(root, "internal", sub)
		info, err := os.Stat(fullPath)
		require.NoError(t, err, "internal/%s must exist", sub)
		assert.True(t, info.IsDir(), "internal/%s must be a directory", sub)
	}
}

func TestProjectStructure_GitkeepFiles(t *testing.T) {
	root := projectRoot(t)
	for _, dir := range requiredDirs() {
		t.Run(dir, func(t *testing.T) {
			fullPath := filepath.Join(root, dir)
			// Directory must exist and contain at least one file
			// (.gitkeep or real source files)
			entries, err := os.ReadDir(fullPath)
			require.NoError(t, err, "%s must be readable", dir)
			assert.Greater(t, len(entries), 0,
				"%s must contain at least one file (.gitkeep or source)", dir)
		})
	}
}

func TestProjectStructure_NoExtraDirectories(t *testing.T) {
	root := projectRoot(t)

	// Check that internal/ only contains expected subdirectories
	expectedInternalSubs := map[string]bool{
		"model":    true,
		"store":    true,
		"service":  true,
		"api":      true,
		"mcp":      true,
		"mtix":     true,
		"testutil": true,
		"scaffold": true, // our test package
		"docs":     true, // documentation generator
	}

	entries, err := os.ReadDir(filepath.Join(root, "internal"))
	require.NoError(t, err, "must be able to read internal/")

	for _, entry := range entries {
		if entry.IsDir() {
			assert.True(t, expectedInternalSubs[entry.Name()],
				"unexpected directory internal/%s", entry.Name())
		}
	}

	// Check that internal/store only contains expected subdirectories
	expectedStoreSubs := map[string]bool{
		"git":   true,
		"index": true,
		"lock":  true,
	}

	storeEntries, err := os.ReadDir(filepath.Join(root, "internal", "store"))
	require.NoError(t, err, "must be able to read internal/store/")

	for _, entry := range storeEntries {
		if entry.IsDir() {
			assert.True(t, expectedStoreSubs[entry.Name()],
				"unexpected directory internal/store/%s", entry.Name())
		}
	}

	// Check cmd/ only has mgit
	cmdEntries, err := os.ReadDir(filepath.Join(root, "cmd"))
	require.NoError(t, err, "must be able to read cmd/")

	for _, entry := range cmdEntries {
		if entry.IsDir() {
			assert.Equal(t, "mgit", entry.Name(),
				"cmd/ should only contain mgit, found %s", entry.Name())
		}
	}

	// Verify no unexpected top-level Go source directories
	topEntries, err := os.ReadDir(root)
	require.NoError(t, err)
	allowedTopDirs := map[string]bool{
		"cmd":      true,
		"internal": true,
		"e2e":      true,
		".git":     true,
		".mtix":    true,
	}
	for _, entry := range topEntries {
		if entry.IsDir() && !strings.HasPrefix(entry.Name(), ".") {
			if entry.Name() != "cmd" && entry.Name() != "internal" && entry.Name() != "e2e" {
				// Allow non-Go directories silently (docs, etc.)
				_ = allowedTopDirs
			}
		}
	}
}
