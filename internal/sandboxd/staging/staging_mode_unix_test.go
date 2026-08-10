//go:build unix

package staging

import (
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBuild_RestrictiveHostUmask_StillDeliversTheWorktreeMode is the inbound
// twin of the outbound defect MGIT-81 fixed.
//
// Staging copies with O_CREATE, whose mode argument the kernel masks with the
// CALLING process's umask. mgit-sandboxd is a long-lived daemon and does not
// control the umask it inherits, so under 0077 a host 0755 file would be
// delivered into the guest at 0700 — an executable the agent then cannot run,
// with nothing reporting that the mode changed. Nothing asserted the delivered
// mode before, which is why it went unnoticed in both directions.
//
// The source tree is built BEFORE the umask changes, so the mode being copied
// really is 0755 and the only thing under test is the copy. Refs: MGIT-81, FR-17.3
func TestBuild_RestrictiveHostUmask_StillDeliversTheWorktreeMode(t *testing.T) {
	worktree := t.TempDir()
	script := filepath.Join(worktree, "build.sh")
	require.NoError(t, os.WriteFile(script, []byte("#!/bin/sh\necho hi\n"), 0o600))
	//nolint:gosec // G302: 0755 IS the fixture — the whole point is that an
	// executable's mode must survive delivery into the guest.
	require.NoError(t, os.Chmod(script, 0o755))

	privateStore := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(privateStore, "HEAD"), []byte("ref: refs/heads/task"), 0o600))
	staged := filepath.Join(t.TempDir(), "worktree-staging")

	old := syscall.Umask(0o077)
	defer syscall.Umask(old)

	require.NoError(t, Build(worktree, privateStore, staged))

	got, err := os.Lstat(filepath.Join(staged, "build.sh"))
	require.NoError(t, err)
	assert.Equal(t, fs.FileMode(0o755), got.Mode().Perm(),
		"the guest must receive the mode the host worktree has, not one the daemon's umask shaved off")
}
