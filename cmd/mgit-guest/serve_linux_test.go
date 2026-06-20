//go:build linux

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withCmdline points procCmdline at a fixture file holding the given
// kernel command line, restoring it after the test.
func withCmdline(t *testing.T, contents string) {
	t.Helper()
	f := filepath.Join(t.TempDir(), "cmdline")
	require.NoError(t, os.WriteFile(f, []byte(contents), 0o600))
	orig := procCmdline
	procCmdline = f
	t.Cleanup(func() { procCmdline = orig })
}

// TestMountWorktree_EmptyDescriptor_NoMount verifies that with no worktree
// descriptor on the cmdline, mountWorktree is a no-op (returns nil without
// attempting a mount) — the no-worktree sandbox case.
func TestMountWorktree_EmptyDescriptor_NoMount(t *testing.T) {
	withCmdline(t, "console=ttyS0 root=/dev/vda")
	assert.NoError(t, mountWorktree())
}

// TestMountWorktree_InvalidDescriptor_FailsClosed verifies a partial
// descriptor (missing source) is rejected before any mount is attempted.
func TestMountWorktree_InvalidDescriptor_FailsClosed(t *testing.T) {
	withCmdline(t, "mgit.worktree=/home/dev/wt mgit.worktree_fs=ext4")
	err := mountWorktree()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "incomplete worktree mount descriptor")
}

// TestMountWorktree_CmdlineUnreadable_FailsClosed verifies an unreadable
// kernel cmdline fails closed rather than silently skipping the worktree.
func TestMountWorktree_CmdlineUnreadable_FailsClosed(t *testing.T) {
	orig := procCmdline
	procCmdline = filepath.Join(t.TempDir(), "no-such-cmdline")
	t.Cleanup(func() { procCmdline = orig })
	assert.Error(t, mountWorktree())
}
