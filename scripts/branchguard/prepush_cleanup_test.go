package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The pre-push hook builds the guard into a scratch directory and must clean
// it up WITHOUT a recursive delete: the lab rule for agents that push through
// this hook is file-by-file removal, and the scratch holds exactly one file.
// Refs: MGIT-318

var recursiveRM = regexp.MustCompile(`\brm\s+-[a-zA-Z]*[rR]`)

func TestPrePushHook_CleansUpWithoutARecursiveDelete(t *testing.T) {
	hook, err := os.ReadFile(filepath.Join("..", "hooks", "pre-push"))
	require.NoError(t, err)
	for i, line := range strings.Split(string(hook), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		assert.False(t, recursiveRM.MatchString(line), "scripts/hooks/pre-push:%d must not delete recursively: %s", i+1, line)
	}
}

func TestPrePushHook_LeavesNoScratchBehind(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not on PATH")
	}
	hook, err := filepath.Abs(filepath.Join("..", "hooks", "pre-push"))
	require.NoError(t, err)
	repo := t.TempDir()
	gitIn(t, repo, "init", "-q")
	gitIn(t, repo, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "root")
	scratch := t.TempDir() // the hook's mktemp -d lands here

	// One pushed branch makes the hook build the guard into its scratch. The
	// repository has no ./scripts/branchguard, so the build fails and the hook
	// reports it and carries on: the cleanup is what is under test.
	cmd := exec.Command("bash", hook) //nolint:gosec // fixed argv, test-only
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "TMPDIR="+scratch)
	cmd.Stdin = strings.NewReader("refs/heads/main 1111111111111111111111111111111111111111 refs/heads/main 0000000000000000000000000000000000000000\n")
	out, runErr := cmd.CombinedOutput()
	require.NoError(t, runErr, "the hook ran: %s", out)

	left, err := os.ReadDir(scratch)
	require.NoError(t, err)
	assert.Empty(t, left, "the hook's scratch directory must be removed when it exits")
}
