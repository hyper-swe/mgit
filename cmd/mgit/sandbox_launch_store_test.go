package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd"
	"github.com/hyper-swe/mgit/internal/sandboxd/quarantine"
)

// storeLayoutManager answers the layout question the way the microVM backend
// does: the quarantine's own check against the repository's shared store.
type storeLayoutManager struct {
	recordingManager
	shared string
}

func (m *storeLayoutManager) CheckWorktreeLayout(worktreePath string) error {
	return quarantine.CheckSharedStore(worktreePath, m.shared)
}

// REFUSED BEFORE ANYTHING IS WRITTEN (MGIT-222). `sandbox launch --worktree
// <the repository root>` registered, reported created, and in the same second
// appended mgit's generated block to the project's TRACKED CLAUDE.md and
// created AGENTS.md; only the first boot refused, and `sandbox remove` left
// the block behind. The whole path runs here as a user runs it: the CLI, the
// control protocol, a real daemon, the service, and the backend wrapped in the
// ceiling exactly as the daemon wires it. A worktree that holds the store is
// refused, and the project is byte-identical afterwards. Refs: MGIT-222,
// MGIT-80, SEC-03, MGIT-251
func TestSandboxLaunchCLI_AWorktreeThatHoldsTheStore_RefusedBeforeAnyWrite(t *testing.T) {
	repo := projectWithGit(t)
	rules := []byte("# project rules\n\nkeep this file exactly as it is\n")
	require.NoError(t, os.WriteFile(filepath.Join(repo, "CLAUDE.md"), rules, 0o600))
	mgr := &storeLayoutManager{shared: filepath.Join(repo, ".mgit")}
	connect := startResourceDaemon(t, model.DefaultSandboxPolicy(), sandboxd.NewCeilingManager(mgr, 8, 0, 0))
	image := "base@sha256:" + strings.Repeat("d", 64)

	for i, worktree := range []string{repo, filepath.Dir(repo)} {
		task := []string{"MGIT-222", "MGIT-222.1"}[i]
		out, err := runSandbox(connect, "launch", "--task-id", task, "--worktree", worktree, "--image", image)
		// assert, not require: with the refusal removed, the tracked-file
		// assertions below must still run and fail (the delete-subject).
		assert.Error(t, err, "a worktree holding the store is refused: %s", out)
		msg := out
		if err != nil {
			msg += err.Error()
		}
		assert.Contains(t, msg, filepath.Join(filepath.Base(repo), ".mgit"), "the refusal names the store")
		assert.Contains(t, msg, "mgit work", "the refusal names the remedy")

		got, readErr := os.ReadFile(filepath.Join(repo, "CLAUDE.md")) //nolint:gosec // G304: this test's own fixture
		require.NoError(t, readErr)
		assert.Equal(t, string(rules), string(got), "the project's CLAUDE.md is byte-identical")
		assert.NoFileExists(t, filepath.Join(repo, "AGENTS.md"), "no AGENTS.md was created")
		assert.NoFileExists(t, filepath.Join(worktree, ".mgit", "sandbox-owner"), "no owner marker was written")
	}
	list, err := runSandbox(connect, "list")
	require.NoError(t, err)
	assert.Contains(t, list, "no sandboxes", "nothing was registered")

	outside := t.TempDir()
	_, err = runSandbox(connect, "launch", "--task-id", "MGIT-222.2", "--worktree", outside, "--image", image)
	require.NoError(t, err, "a worktree outside the repository registers as before")
	cl, err := connect(context.Background())
	require.NoError(t, err)
	_, err = cl.Status(context.Background(), "MGIT-222.2")
	require.NoError(t, err)
}
