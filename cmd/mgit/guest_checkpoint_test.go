package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/sandboxd/provision"
	"github.com/hyper-swe/mgit/internal/sandboxd/staging"
)

// The product claim MGIT-61.7 exists to make true: an agent working inside
// the sandbox can run the checkpoint loop. Its shell is routed into the
// microVM, so `mgit commit` executes THERE — against the SEC-03 private
// store delivered at <worktree>/.mgit, never the host's shared store.
//
// These tests build the exact tree the backends deliver (provision the
// private store -> staging.Build) and drive the real CLI commands over it.
// SCOPE, stated honestly: this proves store RESOLUTION and the command
// wiring on the delivered layout. It is not a booted guest — the in-VM run
// is MGIT-61.10's job. Refs: MGIT-61.7, SEC-03, FR-17.3

// hostRepoWithCommit creates a host mgit repository holding one commit on a
// task branch, and returns its root. It stands in for the developer's real
// repo, whose .mgit is the SHARED store the guest must never reach.
func hostRepoWithCommit(t *testing.T, taskID string) string {
	t.Helper()
	repo := t.TempDir()
	t.Chdir(repo)

	require.NoError(t, runCLI(t, "init"), "mgit init")
	require.NoError(t, os.WriteFile(filepath.Join(repo, "seed.txt"), []byte("seed\n"), 0o600))
	require.NoError(t, runCLI(t, "add", "seed.txt"))
	require.NoError(t, runCLI(t, "commit", "-m", "seed the repo", "--task", taskID))
	// The provisioner seeds the guest from the TASK BRANCH tip, and task
	// branches are created by squash, not by commit (MGIT-22) — so a launch
	// for a task with no squash yet has nothing to seed from.
	require.NoError(t, runCLI(t, "squash", "--task-id", taskID), "create the task branch to seed from")
	return repo
}

// runCLI executes one mgit command through the real root command.
func runCLI(t *testing.T, args ...string) error {
	t.Helper()
	var out bytes.Buffer
	root := rootCmd()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.Execute()
	if err != nil {
		t.Logf("mgit %s: %v\n%s", strings.Join(args, " "), err, out.String())
	}
	return err
}

// runCLIOut executes one mgit command and returns everything it printed.
//
// It captures the process's real stdout, not cobra's writer: the commands
// print through fmt.Println to os.Stdout, so a cobra SetOut buffer comes back
// empty and every output assertion would pass vacuously.
func runCLIOut(t *testing.T, args ...string) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	require.NoError(t, err)

	saved := os.Stdout
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()

	var cobraOut bytes.Buffer
	root := rootCmd()
	root.SetOut(&cobraOut)
	root.SetErr(&cobraOut)
	root.SetArgs(args)
	runErr := root.Execute()

	os.Stdout = saved
	require.NoError(t, w.Close())
	out := <-done + cobraOut.String()
	require.NoError(t, r.Close())
	return out, runErr
}

// deliverGuestTree builds the tree a backend shares into the guest: the
// worktree files, with the per-task PRIVATE store laid in at .mgit and the
// host's own store excluded. Returns the guest-side worktree root.
func deliverGuestTree(t *testing.T, hostRepo, taskID string) string {
	t.Helper()
	prov, err := provision.NewStoreProvisioner(hostRepo)
	require.NoError(t, err, "private-store provisioner")

	privDir := filepath.Join(t.TempDir(), "private-store")
	store, err := prov.Provision(taskID, privDir)
	require.NoError(t, err, "provision the SEC-03 private store")

	guestTree := filepath.Join(t.TempDir(), "staged")
	require.NoError(t, staging.Build(hostRepo, store.Dir, guestTree), "stage the guest worktree")
	return guestTree
}

func TestGuest_CommitStatusLog_WorkAgainstThePrivateStore(t *testing.T) {
	const taskID = "MGIT-61.7"
	hostRepo := hostRepoWithCommit(t, taskID)
	guestTree := deliverGuestTree(t, hostRepo, taskID)

	// Everything below runs as the guest sees it: inside the delivered tree,
	// with the in-sandbox marker set.
	t.Setenv(guestModeEnv, "1")
	t.Chdir(guestTree)

	require.NoError(t, os.WriteFile(filepath.Join(guestTree, "agent-work.txt"),
		[]byte("work the agent did in the sandbox\n"), 0o600))

	require.NoError(t, runCLI(t, "add", "agent-work.txt"), "agents must be able to stage in-guest")
	require.NoError(t, runCLI(t, "commit", "-m", "in-sandbox checkpoint", "--task", taskID),
		"THE product claim: an agent can checkpoint from inside the sandbox")

	logOut, err := runCLIOut(t, "log")
	require.NoError(t, err, "mgit log in-guest")
	assert.Contains(t, logOut, "in-sandbox checkpoint",
		"the in-guest commit must be visible in the guest's own history")

	statusOut, err := runCLIOut(t, "status")
	require.NoError(t, err, "mgit status in-guest")
	assert.NotContains(t, statusOut, "agent-work.txt",
		"the committed file should no longer be reported as pending")
}

func TestGuest_PrivateStoreIsTheOnlyStore_HostSharedStoreUnreachable(t *testing.T) {
	const taskID = "MGIT-61.7"
	hostRepo := hostRepoWithCommit(t, taskID)

	// A marker only the HOST's shared store carries. If the guest's mgit
	// could reach that store, this file would be inside the delivered tree.
	hostOnlyMarker := filepath.Join(hostRepo, ".mgit", "host-only-marker")
	require.NoError(t, os.WriteFile(hostOnlyMarker, []byte("host"), 0o600))

	guestTree := deliverGuestTree(t, hostRepo, taskID)

	guestStore := filepath.Join(guestTree, staging.GuestStoreName)
	info, err := os.Stat(guestStore)
	require.NoError(t, err, "the guest must get a store to commit into at %s", staging.GuestStoreName)
	assert.True(t, info.IsDir(), "the guest store must be a real directory, not a worktree marker "+
		"pointing at a host path that does not exist in the guest")

	_, err = os.Stat(filepath.Join(guestStore, "host-only-marker"))
	assert.True(t, os.IsNotExist(err),
		"the host shared store leaked into the guest's store (SEC-03)")

	// And the resolution the CLI itself performs lands on the private store,
	// not on any ancestor of the staging dir.
	t.Chdir(guestTree)
	root, err := findRepoRoot(guestTree)
	require.NoError(t, err)
	assert.Equal(t, guestTree, root,
		"the guest's mgit must resolve its own delivered store, not an ancestor's")
}

func TestGuest_CommitsAreIsolatedFromTheHostStore(t *testing.T) {
	const taskID = "MGIT-61.7"
	hostRepo := hostRepoWithCommit(t, taskID)
	guestTree := deliverGuestTree(t, hostRepo, taskID)

	t.Setenv(guestModeEnv, "1")
	t.Chdir(guestTree)
	require.NoError(t, os.WriteFile(filepath.Join(guestTree, "sandbox-only.txt"), []byte("x\n"), 0o600))
	require.NoError(t, runCLI(t, "add", "sandbox-only.txt"))
	require.NoError(t, runCLI(t, "commit", "-m", "sandbox-only commit", "--task", taskID))

	// Back on the host: the guest's commit must NOT be there. Land is the
	// only bridge, and it is host-initiated and verified (SEC-01/SEC-03).
	t.Chdir(hostRepo)
	hostLog, err := runCLIOut(t, "log")
	require.NoError(t, err)
	assert.NotContains(t, hostLog, "sandbox-only commit",
		"a guest commit reached the host store without going through the verified land path (SEC-03)")
}
