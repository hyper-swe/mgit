//go:build linux

package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/guestboot"
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

// errStopAfterScratch halts makeRootWritableWith right after the scratch
// mount so the test exercises the disk-vs-tmpfs selection without the
// privileged overlay/switch_root that follows.
var errStopAfterScratch = errors.New("stop after scratch")

// recordingMounter records the descriptor it was handed and the disk-backed
// verdict it would report, then returns a sentinel error so the privileged
// steps after the scratch mount never run.
type recordingMounter struct {
	got        guestboot.OverlayUpper
	diskBacked bool
	called     bool
}

func (m *recordingMounter) mountScratch(o guestboot.OverlayUpper) (bool, error) {
	m.called = true
	m.got = o
	return m.diskBacked, errStopAfterScratch
}

// TestMakeRootWritable_OverlayDeviceSelection verifies makeRootWritableWith
// hands the parsed overlay descriptor to the scratch mounter — a valid disk
// device is passed through (disk-backed upper) and an absent/partial one
// yields an empty descriptor (tmpfs fallback). The privileged overlay +
// switch_root that follows is e2e-gated, so the mounter short-circuits.
func TestMakeRootWritable_OverlayDeviceSelection(t *testing.T) {
	tests := []struct {
		name      string
		desc      guestboot.OverlayUpper
		wantValid bool
	}{
		{"disk_overlay_drive", guestboot.OverlayUpper{Device: "/dev/vdb", FSType: "ext4"}, true},
		{"no_device_tmpfs_fallback", guestboot.OverlayUpper{}, false},
		{"partial_device_only", guestboot.OverlayUpper{Device: "/dev/vdb"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &recordingMounter{}
			err := makeRootWritableWith(m, tt.desc)
			require.ErrorIs(t, err, errStopAfterScratch)
			assert.True(t, m.called, "scratch mounter must be consulted")
			assert.Equal(t, tt.desc, m.got, "the parsed descriptor is passed through unchanged")
			assert.Equal(t, tt.wantValid, m.got.Valid())
		})
	}
}

// TestMakeRootWritable_CmdlineUnreadable_FailsClosed verifies that an
// unreadable kernel cmdline fails the writable-root setup closed rather
// than silently proceeding without an overlay descriptor.
func TestMakeRootWritable_CmdlineUnreadable_FailsClosed(t *testing.T) {
	orig := procCmdline
	procCmdline = filepath.Join(t.TempDir(), "no-such-cmdline")
	t.Cleanup(func() { procCmdline = orig })
	err := makeRootWritable()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "read kernel cmdline")
}
