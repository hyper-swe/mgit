//go:build darwin || linux

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/hyper-swe/mgit/internal/model"
	gitstore "github.com/hyper-swe/mgit/internal/store/git"
	"github.com/hyper-swe/mgit/internal/store/index"
	"github.com/stretchr/testify/require"
)

// Run the diagnostic's recovery in a NEW repository. An old linked worktree
// cannot simply be committed under a different task ID. Never copy its store.
func TestContentRecovery_LegacyCLIRefusalsAndNewRepository(t *testing.T) {
	_, source, _, ok := runtime.Caller(0)
	require.True(t, ok)
	sourceRoot := filepath.Clean(filepath.Join(filepath.Dir(source), "..", ".."))
	cli := filepath.Join(t.TempDir(), "mgit")
	build := exec.CommandContext(t.Context(), "go", "build", "-buildvcs=false", "-o", cli, "./cmd/mgit") //nolint:gosec // G204: fixed build target or test-built CLI with fixture-owned arguments
	build.Dir = sourceRoot
	out, err := build.CombinedOutput()
	require.NoError(t, err, string(out))
	root := t.TempDir()
	liveLandCommand(t, root, "git", "init")
	liveLandCommand(t, root, "git", "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "test: base")
	liveLandCommand(t, root, cli, "init")
	liveLandCommand(t, root, cli, "work", "wt", "--task-id", "OLD-1")
	native := filepath.Join(root, "wt")
	repo, err := gitstore.OpenLinked(native, filepath.Join(root, ".mgit"), model.TaskBranchName("OLD-1"), time.Now)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(native, "inspected.txt"), []byte("inspected host bytes\n"), 0600))
	require.NoError(t, gitstore.NewWorktreeStore(repo).Add(context.Background(), "inspected.txt"))
	hash, err := gitstore.NewCommitStore(repo).CreateCommit(context.Background(), &model.Commit{AgentID: "test", Message: "[MGIT:OLD-1] legacy host work"})
	require.NoError(t, err)
	require.NoError(t, repo.Close())
	idx, err := index.New(filepath.Join(root, ".mgit", "index.db"), time.Now)
	require.NoError(t, err)
	// Generic/imported and pre-upgrade rows deliberately have no host admission.
	require.NoError(t, idx.AppendTaskCommit(context.Background(), index.TaskCommitInsert{TaskID: "OLD-1", CommitHash: hash, ContentHash: "legacy"}))
	require.NoError(t, idx.Close())
	binding, err := liveLandCommandResult(t, native, cli, "commit", "-a", "--task-id", "RECOVERY-1", "-m", "Recover inspected files")
	require.Error(t, err)
	require.Contains(t, binding, "bound")
	for name, args := range map[string][]string{
		"squash":     {"squash", "--task-id", "OLD-1", "--to-git"},
		"diff-task":  {"diff", "--task-id", "OLD-1"},
		"export-git": {"export", "OLD-1", "--format", "git"},
		"restore":    {"restore", "inspected.txt", "--commit", hash},
		"diff-hash":  {"diff", "--from", hash, "--to", hash},
	} {
		t.Run(name, func(t *testing.T) {
			refusal, err := liveLandCommandResult(t, native, cli, args...)
			require.Error(t, err)
			require.Contains(t, refusal, "ambiguous content eligibility")
			require.Contains(t, refusal, "mgit init")
			require.Contains(t, refusal, "mgit commit -a --task-id RECOVERY-1 -m 'Recover inspected files'")
			recovery := t.TempDir() // Outside the old task-bound worktree, initially EMPTY.
			liveLandCommand(t, recovery, cli, "init")
			inspected, err := os.ReadFile(filepath.Join(native, "inspected.txt")) //nolint:gosec // G304: fixed file in this test-owned native fixture or its backup
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(recovery, "inspected.txt"), inspected, 0600)) //nolint:gosec // G703: fixed filename under a test-owned empty directory
			liveLandCommand(t, recovery, cli, "commit", "-a", "--task-id", "RECOVERY-1", "-m", "Recover inspected files")
			patch := liveLandCommand(t, recovery, cli, "export", "RECOVERY-1", "--format", "git")
			require.Contains(t, patch, "+inspected host bytes")
			original, err := os.ReadFile(filepath.Join(native, "inspected.txt")) //nolint:gosec // G304: fixed file in this test-owned native fixture or its backup
			require.NoError(t, err)
			require.Equal(t, inspected, original)
		})
	}
}
