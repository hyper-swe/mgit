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

// TestPublishPortsRoundTrip verifies the host-appended published-ports
// descriptor parses back to the same guest ports on the guest — the contract
// both ends share for the SEC-09 vsock<->TCP bridge.
func TestPublishPortsRoundTrip(t *testing.T) {
	ports := []int{3000, 8080, 1}
	cmdline := AppendPublishPortsCmdline("console=ttyS0 reboot=k", ports)
	assert.Equal(t, ports, ParsePublishPorts(cmdline))
}

// TestAppendPublishPortsCmdline_PreservesBase verifies base args are kept and
// the descriptor is appended as a single comma-joined token after them.
func TestAppendPublishPortsCmdline_PreservesBase(t *testing.T) {
	out := AppendPublishPortsCmdline("console=ttyS0 panic=1", []int{3000, 8080})
	assert.Contains(t, out, "console=ttyS0 panic=1 ")
	assert.Contains(t, out, "mgit.publish_ports=3000,8080")
}

// TestAppendPublishPortsCmdline_EmptyBase verifies no leading space on a blank base.
func TestAppendPublishPortsCmdline_EmptyBase(t *testing.T) {
	out := AppendPublishPortsCmdline("", []int{3000})
	assert.Equal(t, "mgit.publish_ports=3000", out)
}

// TestAppendPublishPortsCmdline_NoPorts_AddsNothing verifies an empty (or
// all-invalid) port list leaves the cmdline untouched.
func TestAppendPublishPortsCmdline_NoPorts_AddsNothing(t *testing.T) {
	assert.Equal(t, "console=ttyS0", AppendPublishPortsCmdline("console=ttyS0", nil))
	assert.Equal(t, "console=ttyS0", AppendPublishPortsCmdline("console=ttyS0", []int{0, 70000, -1}))
}

// TestAppendPublishPortsCmdline_DropsOutOfRange verifies out-of-range ports
// are dropped so the descriptor only ever names valid guest TCP ports.
func TestAppendPublishPortsCmdline_DropsOutOfRange(t *testing.T) {
	out := AppendPublishPortsCmdline("", []int{0, 3000, 70000, 8080})
	assert.Equal(t, "mgit.publish_ports=3000,8080", out)
}

// TestParsePublishPorts_IgnoresUnrelatedTokens verifies only the publish key
// is extracted from a realistic cmdline that also carries other descriptors.
func TestParsePublishPorts_IgnoresUnrelatedTokens(t *testing.T) {
	cmdline := "console=ttyS0 mgit.worktree=/wt mgit.publish_ports=3000,8080 root=/dev/vda mgit.overlay_dev=/dev/vdb"
	assert.Equal(t, []int{3000, 8080}, ParsePublishPorts(cmdline))
}

// TestParsePublishPorts_Empty verifies an absent descriptor yields no ports.
func TestParsePublishPorts_Empty(t *testing.T) {
	assert.Empty(t, ParsePublishPorts("console=ttyS0 root=/dev/vda"))
}

// TestParsePublishPorts_SkipsMalformed verifies malformed/out-of-range
// entries are skipped (the guest only listens on valid ports).
func TestParsePublishPorts_SkipsMalformed(t *testing.T) {
	assert.Equal(t, []int{3000, 8080}, ParsePublishPorts("mgit.publish_ports=3000,abc,70000,0,8080"))
}

// TestBootTokens_MergesTheEnvChannelWithTheCmdline covers the libkrun case:
// that backend boots libkrunfw's own kernel and never composes a command
// line, so the host has no cmdline to append the FR-17.3 worktree descriptor
// to. It supplies the SAME tokens through the guest environment instead, and
// the guest parses one merged string — so every existing descriptor parser is
// reused verbatim rather than gaining a second format. Refs: FR-17.3, ADR-010
func TestBootTokens_MergesTheEnvChannelWithTheCmdline(t *testing.T) {
	tests := []struct {
		name         string
		cmdline, env string
		wantPath     string
		wantSource   string
	}{
		{
			name:     "cmdline_only_firecracker_and_vzf",
			cmdline:  "console=ttyS0 mgit.worktree=/w mgit.worktree_fs=ext4 mgit.worktree_src=/dev/vdc",
			wantPath: "/w", wantSource: "/dev/vdc",
		},
		{
			// libkrun: nothing of ours on the cmdline, everything in the env.
			name:     "env_only_libkrun",
			cmdline:  "console=hvc0 root=/dev/root",
			env:      "mgit.worktree=/work/wt mgit.worktree_fs=virtiofs mgit.worktree_src=work",
			wantPath: "/work/wt", wantSource: "work",
		},
		{
			name: "neither_yields_no_descriptor",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseWorktreeMount(BootTokens(tt.cmdline, tt.env))
			if got.Path != tt.wantPath || got.Source != tt.wantSource {
				t.Errorf("descriptor = %+v, want path %q source %q", got, tt.wantPath, tt.wantSource)
			}
		})
	}
}

func TestBootTokens_CmdlineWins_SoAGuestEnvCannotRedirectTheMount(t *testing.T) {
	// The env channel is host-supplied like the cmdline, but the cmdline is
	// the harder-to-reach one; if both carry a descriptor the cmdline must
	// win, so a backend that has a real cmdline is never overridden.
	merged := BootTokens(
		"mgit.worktree=/real mgit.worktree_fs=ext4 mgit.worktree_src=/dev/vdc",
		"mgit.worktree=/evil mgit.worktree_fs=virtiofs mgit.worktree_src=evil",
	)
	if got := ParseWorktreeMount(merged); got.Path != "/real" || got.Source != "/dev/vdc" {
		t.Errorf("descriptor = %+v, want the cmdline's", got)
	}
}
