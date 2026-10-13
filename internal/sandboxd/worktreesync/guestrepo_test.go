package worktreesync

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/sandboxd/staging"
)

// FEAT-6.114.1: the repository the guest is given is the guest's. Sync never
// reads it as host content, never deletes it, and never reports the guest's
// own commits as a conflict.
func TestSync_TheGuestsOwnRepoIsLeftAlone(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	f := fixture{worktree: t.TempDir(), store: t.TempDir(), stateDir: t.TempDir()}
	f.guestTree = filepath.Join(f.stateDir, "worktree-staging")
	writeTree(t, f.worktree, map[string]string{"app.go": "V1"})
	require.NoError(t, os.WriteFile(filepath.Join(f.store, "HEAD"), []byte("ref: refs/heads/task"), 0o600))
	require.NoError(t, staging.BuildForGuest(f.worktree, f.store, f.guestTree, true))
	require.NoError(t, RecordDelivery(f.guestTree, f.stateDir))
	_, err := os.Lstat(filepath.Join(f.guestTree, ".git"))
	require.NoError(t, err, "the guest has its repo")

	// the guest commits something of its own
	writeTree(t, f.guestTree, map[string]string{"guest-notes.txt": "mine"})
	for _, args := range [][]string{{"add", "-A"}, {"-c", "user.name=g", "-c", "user.email=g@g", "commit", "-qm", "guest work"}} {
		out, cerr := exec.Command("git", append([]string{"-C", f.guestTree}, args...)...).CombinedOutput()
		require.NoError(t, cerr, string(out))
	}

	res, err := f.sync(false)
	require.NoError(t, err)
	assert.True(t, res.Skipped, "an unchanged host is skipped: %+v", res)
	assert.Empty(t, res.Deleted)

	writeTree(t, f.worktree, map[string]string{"app.go": "V2"})
	res, err = f.sync(false)
	require.NoError(t, err)
	assert.Equal(t, []string{"app.go"}, res.Updated)
	assert.Empty(t, res.Deleted, "the guest's repo is not deleted by a sync")
	out, gerr := exec.Command("git", "-C", f.guestTree, "log", "--format=%s").CombinedOutput()
	require.NoError(t, gerr)
	assert.Contains(t, string(out), "guest work", "the guest's commits survive a sync")
}
