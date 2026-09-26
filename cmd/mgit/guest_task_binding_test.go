package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// A GUEST COMMIT INHERITS THE WORKTREE'S TASK (MGIT-256). On the host a
// linked worktree's .mgit is a marker naming the shared store and the bound
// task, so `mgit commit` needs no --task-id. Inside the sandbox that .mgit is
// the private store (SEC-03), which carried no binding, so the guest refused
// every commit without --task-id, while the CLAUDE.md block mgit generates
// into the same worktree says the task ID is inherited. The marker cannot be
// copied in: it names the host's shared store, which the guest must never
// resolve.
//
// The tree below is the one a backend delivers (the private store
// provisioned for the worktree, then staged), as in guest_generated_files_test.go.
// Refs: MGIT-256, MGIT-236, SEC-03, FR-16
func TestGuest_CommitInheritsTheWorktreesTask(t *testing.T) {
	const taskID = "MGIT-256"
	guestTree := guestTreeForTask(t, taskID)
	require.NoError(t, os.WriteFile(filepath.Join(guestTree, "step.txt"), []byte("a contained step\n"), 0o600))

	require.NoError(t, runCLI(t, "commit", "-a", "-m", "a contained step"),
		"a guest commit in a task's worktree needs no --task-id, as the generated guidance says")

	out, _, err := runCLICap(t, "log", "--json", "-n", "1")
	require.NoError(t, err)
	var commits []model.Commit
	require.NoError(t, json.Unmarshal([]byte(out), &commits), "log --json output:\n%s", out)
	require.Len(t, commits, 1)
	assert.Equal(t, taskID, commits[0].TaskID.String(), "the commit is tagged with the worktree's task")
	assert.Contains(t, commits[0].Message, "a contained step")
}

// The binding refuses a contradicting --task-id in the guest as it does on the
// host: a commit is never silently attributed to another task. Refs: MGIT-256, MGIT-24
func TestGuest_CommitWithAnotherTaskID_Refused(t *testing.T) {
	guestTree := guestTreeForTask(t, "MGIT-256")
	require.NoError(t, os.WriteFile(filepath.Join(guestTree, "step.txt"), []byte("x\n"), 0o600))

	err := runCLI(t, "commit", "-a", "-m", "wrong task", "--task-id", "MGIT-999")

	require.ErrorIs(t, err, model.ErrTaskMismatch)
	assert.Contains(t, err.Error(), "MGIT-256")
}

// What reaches the guest names the task and nothing of the host: no file in
// the delivered private store holds the host repository's path, so the guest
// learns its task without learning where the shared store is. Refs: MGIT-256, SEC-03
func TestGuest_TaskBindingCarriesNoHostPath(t *testing.T) {
	hostRepo := hostRepoWithCommit(t, "MGIT-256")
	worktree := filepath.Join(t.TempDir(), "wt")
	require.NoError(t, runCLI(t, "worktree", "add", worktree, "--task-id", "MGIT-256"))
	guestTree := deliverWorktreeToGuest(t, hostRepo, worktree, "MGIT-256")

	err := filepath.WalkDir(filepath.Join(guestTree, ".mgit"), func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path) //nolint:gosec // test-controlled tree
		if err != nil {
			return err
		}
		for _, host := range []string{hostRepo, worktree} {
			assert.False(t, strings.Contains(string(data), host), "%s names the host path %s", path, host)
		}
		return nil
	})
	require.NoError(t, err)
}

// guestTreeForTask delivers a task's worktree to an in-process guest and
// enters it in guest mode.
func guestTreeForTask(t *testing.T, taskID string) string {
	t.Helper()
	hostRepo := hostRepoWithCommit(t, taskID)
	worktree := filepath.Join(t.TempDir(), "wt")
	require.NoError(t, runCLI(t, "worktree", "add", worktree, "--task-id", taskID))
	guestTree := deliverWorktreeToGuest(t, hostRepo, worktree, taskID)
	t.Setenv(guestModeEnv, "1")
	t.Chdir(guestTree)
	return guestTree
}
