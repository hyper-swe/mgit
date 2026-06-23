package guestboot

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestRoundTrip verifies a descriptor appended by the host parses back
// identically on the guest — the contract both ends share.
func TestRoundTrip(t *testing.T) {
	for name, w := range map[string]WorktreeMount{
		"virtiofs": {Path: "/home/dev/repo/worktrees/task-a", FSType: "virtiofs", Source: "work"},
		"ext4":     {Path: "/home/dev/repo/worktrees/task-a", FSType: "ext4", Source: "/dev/vdc"},
	} {
		t.Run(name, func(t *testing.T) {
			cmdline := AppendCmdline("console=ttyS0 reboot=k", w)
			got := ParseWorktreeMount(cmdline)
			assert.Equal(t, w, got)
			assert.True(t, got.Valid())
		})
	}
}

// TestAppendCmdline_PreservesBase verifies the base kernel args are kept
// and the descriptor is appended after them.
func TestAppendCmdline_PreservesBase(t *testing.T) {
	w := WorktreeMount{Path: "/wt", FSType: "ext4", Source: "/dev/vdc"}
	out := AppendCmdline("console=ttyS0 panic=1", w)
	assert.True(t, len(out) > len("console=ttyS0 panic=1"))
	assert.Contains(t, out, "console=ttyS0 panic=1 ")
	assert.Contains(t, out, "mgit.worktree=/wt")
	assert.Contains(t, out, "mgit.worktree_fs=ext4")
	assert.Contains(t, out, "mgit.worktree_src=/dev/vdc")
}

// TestAppendCmdline_EmptyBase verifies no leading space when the base is
// blank.
func TestAppendCmdline_EmptyBase(t *testing.T) {
	w := WorktreeMount{Path: "/wt", FSType: "ext4", Source: "/dev/vdc"}
	out := AppendCmdline("", w)
	assert.Equal(t, "mgit.worktree=/wt mgit.worktree_fs=ext4 mgit.worktree_src=/dev/vdc", out)
}

// TestAppendCmdline_NoPath_AddsNothing verifies a descriptor with no path
// (no worktree to deliver) leaves the cmdline untouched.
func TestAppendCmdline_NoPath_AddsNothing(t *testing.T) {
	assert.Equal(t, "console=ttyS0", AppendCmdline("console=ttyS0", WorktreeMount{}))
}

// TestParse_IgnoresUnrelatedTokens verifies only the worktree keys are
// extracted from a realistic kernel cmdline.
func TestParse_IgnoresUnrelatedTokens(t *testing.T) {
	cmdline := "console=ttyS0 reboot=k panic=1 mgit.worktree=/wt root=/dev/vda mgit.worktree_fs=ext4 mgit.worktree_src=/dev/vdc init=/sbin/x"
	got := ParseWorktreeMount(cmdline)
	assert.Equal(t, WorktreeMount{Path: "/wt", FSType: "ext4", Source: "/dev/vdc"}, got)
}

// TestParse_Empty verifies a cmdline with no worktree keys yields an empty
// descriptor.
func TestParse_Empty(t *testing.T) {
	got := ParseWorktreeMount("console=ttyS0 root=/dev/vda")
	assert.True(t, got.Empty())
	assert.False(t, got.Valid())
}

// TestParse_KeyWithoutValue_Skipped verifies a bare key (no =value) is
// ignored rather than setting an empty field.
func TestParse_KeyWithoutValue_Skipped(t *testing.T) {
	got := ParseWorktreeMount("mgit.worktree mgit.worktree_fs= mgit.worktree_src=/dev/vdc")
	assert.Equal(t, WorktreeMount{Source: "/dev/vdc"}, got)
	assert.False(t, got.Valid())
}

// TestOverlayRoundTrip verifies an overlay-upper descriptor appended by
// the host parses back identically on the guest.
func TestOverlayRoundTrip(t *testing.T) {
	o := OverlayUpper{Device: "/dev/vdb", FSType: "ext4"}
	cmdline := AppendOverlayCmdline("console=ttyS0 reboot=k", o)
	got := ParseOverlayUpper(cmdline)
	assert.Equal(t, o, got)
	assert.True(t, got.Valid())
}

// TestAppendOverlayCmdline_PreservesBase verifies base args are kept and
// the overlay descriptor is appended after them.
func TestAppendOverlayCmdline_PreservesBase(t *testing.T) {
	out := AppendOverlayCmdline("console=ttyS0 panic=1", OverlayUpper{Device: "/dev/vdb", FSType: "ext4"})
	assert.Contains(t, out, "console=ttyS0 panic=1 ")
	assert.Contains(t, out, "mgit.overlay_dev=/dev/vdb")
	assert.Contains(t, out, "mgit.overlay_fs=ext4")
}

// TestAppendOverlayCmdline_EmptyBase verifies no leading space on a blank base.
func TestAppendOverlayCmdline_EmptyBase(t *testing.T) {
	out := AppendOverlayCmdline("", OverlayUpper{Device: "/dev/vdb", FSType: "ext4"})
	assert.Equal(t, "mgit.overlay_dev=/dev/vdb mgit.overlay_fs=ext4", out)
}

// TestAppendOverlayCmdline_NoDevice_AddsNothing verifies a descriptor with
// no device (no disk overlay attached) leaves the cmdline untouched.
func TestAppendOverlayCmdline_NoDevice_AddsNothing(t *testing.T) {
	assert.Equal(t, "console=ttyS0", AppendOverlayCmdline("console=ttyS0", OverlayUpper{}))
}

// TestParseOverlayUpper_IgnoresUnrelatedTokens verifies only the overlay
// keys are extracted from a realistic cmdline that also carries the
// worktree descriptor.
func TestParseOverlayUpper_IgnoresUnrelatedTokens(t *testing.T) {
	cmdline := "console=ttyS0 mgit.worktree=/wt mgit.overlay_dev=/dev/vdb root=/dev/vda mgit.overlay_fs=ext4 mgit.worktree_src=/dev/vdc"
	got := ParseOverlayUpper(cmdline)
	assert.Equal(t, OverlayUpper{Device: "/dev/vdb", FSType: "ext4"}, got)
}

// TestOverlayValid covers the overlay validity rules.
func TestOverlayValid(t *testing.T) {
	cases := map[string]struct {
		o    OverlayUpper
		want bool
	}{
		"complete":  {OverlayUpper{Device: "/dev/vdb", FSType: "ext4"}, true},
		"no_device": {OverlayUpper{FSType: "ext4"}, false},
		"no_fstype": {OverlayUpper{Device: "/dev/vdb"}, false},
		"empty":     {OverlayUpper{}, false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, c.want, c.o.Valid())
		})
	}
}

// TestValid covers the validity rules: absolute path + all fields.
func TestValid(t *testing.T) {
	cases := map[string]struct {
		w    WorktreeMount
		want bool
	}{
		"complete":      {WorktreeMount{Path: "/wt", FSType: "ext4", Source: "/dev/vdc"}, true},
		"relative_path": {WorktreeMount{Path: "rel", FSType: "ext4", Source: "/dev/vdc"}, false},
		"no_fstype":     {WorktreeMount{Path: "/wt", Source: "/dev/vdc"}, false},
		"no_source":     {WorktreeMount{Path: "/wt", FSType: "ext4"}, false},
		"empty":         {WorktreeMount{}, false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, c.want, c.w.Valid())
		})
	}
}
