package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Refs: FR-2.6, MGIT-288. Drive both printed commands through a POSIX shell,
// not a test's independently quoted copy of the remedy.
func TestE2E_StagedDirectoryRemedies_WorkAsPrinted(t *testing.T) {
	bin := buildMgitBinary(t)
	for _, dir := range []string{"a/b c", "a/b'c", "a/$HOME;echo", "a/*", "-dir"} {
		t.Run(dir, func(t *testing.T) { stagedRemedyScenario(t, bin, dir) })
	}
}

func stagedRemedyScenario(t *testing.T, bin, dir string) {
	t.Helper()
	root := t.TempDir()
	mustMgit(t, bin, root, "init")
	paths := []string{dir, dir + "/file", dir + "x/sibling", "z other", "keep.txt"}
	for _, p := range []string{dir + "/file", dir + "x/sibling", "z other/file", "keep.txt"} {
		require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(root, p)), 0o750))
		require.NoError(t, os.WriteFile(filepath.Join(root, p), []byte(p), 0o600))
	}
	setStagedRemedyPaths(t, root, paths)
	before := mustMgit(t, bin, root, "log", "--json")
	out, err := runMgit(t, bin, root, "commit", "--task-id", "MGIT-288", "-m", "work")
	require.Error(t, err, out)
	assert.Equal(t, before, mustMgit(t, bin, root, "log", "--json"))
	for _, p := range []string{dir, "z other"} {
		assert.Contains(t, out, fmt.Sprintf("%q is a directory", p))
	}
	assert.Contains(t, out, "also unstages the files under it")
	restores := regexp.MustCompile("`(mgit restore --staged [^`]+)`").FindAllStringSubmatch(out, -1)
	adds := regexp.MustCompile("`(mgit add [^`]+)`").FindAllStringSubmatch(out, -1)
	require.Len(t, restores, 2, out)
	require.Len(t, adds, 2, out)
	runPrintedStagingCommand(t, bin, root, restores[0][1])
	assert.ElementsMatch(t, paths[2:], readStagedRemedyPaths(t, root))
	runPrintedStagingCommand(t, bin, root, adds[0][1])
	assert.ElementsMatch(t, append(paths[2:], dir+"/file"), readStagedRemedyPaths(t, root))
	runPrintedStagingCommand(t, bin, root, restores[1][1])
	runPrintedStagingCommand(t, bin, root, adds[1][1])
	assert.ElementsMatch(t, []string{dir + "/file", dir + "x/sibling", "z other/file", "keep.txt"}, readStagedRemedyPaths(t, root))
	mustMgit(t, bin, root, "commit", "--task-id", "MGIT-288", "-m", "recovered")
	assert.Empty(t, readStagedRemedyPaths(t, root))
}

func runPrintedStagingCommand(t *testing.T, bin, root, command string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", `mgit() { "$MGIT_TEST_BINARY" "$@"; }; `+command) //nolint:gosec // Execute the fixture's printed remedy to verify shell parsing.
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "MGIT_TEST_BINARY="+bin)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s: %s", command, out)
}

func setStagedRemedyPaths(t *testing.T, root string, paths []string) {
	t.Helper()
	data, err := json.Marshal(struct {
		Paths []string `json:"paths"`
	}{paths})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(root, ".mgit", "staging.json"), data, 0o600))
}

func readStagedRemedyPaths(t *testing.T, root string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, ".mgit", "staging.json")) //nolint:gosec // Read the staging file in the test-owned root.
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(t, err)
	var staging struct {
		Paths []string `json:"paths"`
	}
	require.NoError(t, json.Unmarshal(data, &staging))
	return staging.Paths
}
