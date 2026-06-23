// Package guestboot is the single source of the host->guest boot contract
// for worktree delivery (FR-17.3). The host (mgit-sandboxd platform
// backends) appends the worktree mount descriptor to the guest kernel
// command line; the guest (mgit-guest, PID 1) parses it and mounts the
// worktree at its identical absolute path. Both ends import this package
// so the cmdline keys cannot drift between producer and consumer — the
// same discipline as the exec wire protocol (internal/execwire).
//
// The descriptor is mechanism-agnostic: vzf delivers the worktree as a
// virtiofs share (a mount tag), firecracker as a virtio-blk ext4 image (a
// device), and the guest mounts whichever it is told (ADR-005 per-backend
// worktree delivery). The cmdline carries no secrets — only the worktree
// path and how to mount it — so it does not weaken the no-host-passthrough
// posture (SEC-01). Refs: FR-17.3, MGIT-11.6.5
package guestboot

import (
	"path/filepath"
	"strings"
)

// Kernel cmdline keys the host appends and the guest parses.
const (
	// KeyWorktreePath is the identical absolute path to mount the worktree at.
	KeyWorktreePath = "mgit.worktree"
	// KeyWorktreeFS is the filesystem type ("virtiofs" or "ext4").
	KeyWorktreeFS = "mgit.worktree_fs"
	// KeyWorktreeSource is the mount source: a virtiofs tag, or a block device.
	KeyWorktreeSource = "mgit.worktree_src"
	// KeyOverlayDev is the block device backing the writable-root overlay
	// upper (the per-VM disk-backed COW drive, e.g. /dev/vdb). Empty/absent
	// means no disk overlay was attached and the guest uses a tmpfs upper.
	KeyOverlayDev = "mgit.overlay_dev"
	// KeyOverlayFS is the filesystem the guest formats/mounts the overlay
	// device with ("ext4"). The drive is delivered as a raw sparse file, so
	// the guest mkfs-es it on first boot if unformatted.
	KeyOverlayFS = "mgit.overlay_fs"
)

// WorktreeMount is the host-supplied worktree delivery descriptor.
type WorktreeMount struct {
	Path   string // identical absolute mount target (the host worktree path)
	FSType string // "virtiofs" (vzf tag share) or "ext4" (firecracker virtio-blk)
	Source string // a virtiofs mount tag, or a block device path (e.g. /dev/vdc)
}

// Valid reports whether the descriptor is fully specified: an absolute
// path plus a filesystem type and source. A guest with an invalid (but
// non-empty) descriptor must fail closed rather than mount something
// unexpected. Refs: FR-17.3
func (w WorktreeMount) Valid() bool {
	return w.Path != "" && filepath.IsAbs(w.Path) && w.FSType != "" && w.Source != ""
}

// Empty reports whether no worktree descriptor was supplied at all (every
// field blank) — distinct from a partially specified, invalid one.
func (w WorktreeMount) Empty() bool {
	return w.Path == "" && w.FSType == "" && w.Source == ""
}

// AppendCmdline returns base with the worktree descriptor appended as
// space-separated key=value pairs the guest will parse. The host calls
// this when building the guest kernel command line. A descriptor with no
// path adds nothing (no worktree to deliver). Refs: FR-17.3, MGIT-11.6.5
func AppendCmdline(base string, w WorktreeMount) string {
	if w.Path == "" {
		return base
	}
	parts := []string{
		KeyWorktreePath + "=" + w.Path,
		KeyWorktreeFS + "=" + w.FSType,
		KeyWorktreeSource + "=" + w.Source,
	}
	suffix := strings.Join(parts, " ")
	if strings.TrimSpace(base) == "" {
		return suffix
	}
	return base + " " + suffix
}

// OverlayUpper is the host-supplied descriptor for the disk-backed
// writable-root overlay upper (FR-17.17/NFR-17.7). When the backend
// attaches a per-VM COW overlay drive, the host names the device and
// filesystem here so the guest backs its overlayfs upperdir with DISK
// (quota-bounded) instead of RAM (tmpfs). The device is supplied on the
// cmdline rather than hardcoded so the upper stays pluggable — the guest
// never assumes /dev/vdb. Refs: FR-17.17, NFR-17.7, MGIT-11.6.7
type OverlayUpper struct {
	Device string // block device backing the upper (e.g. /dev/vdb)
	FSType string // filesystem to format/mount it with (e.g. "ext4")
}

// Valid reports whether the overlay descriptor is fully specified: a
// device path plus a filesystem type. A partial descriptor is treated as
// absent by the guest (tmpfs fallback). Refs: NFR-17.7
func (o OverlayUpper) Valid() bool {
	return o.Device != "" && o.FSType != ""
}

// AppendOverlayCmdline returns base with the overlay-upper descriptor
// appended as space-separated key=value pairs the guest parses. A
// descriptor with no device adds nothing (no disk overlay attached, the
// guest falls back to a tmpfs upper). Refs: NFR-17.7, MGIT-11.6.7
func AppendOverlayCmdline(base string, o OverlayUpper) string {
	if o.Device == "" {
		return base
	}
	suffix := KeyOverlayDev + "=" + o.Device + " " + KeyOverlayFS + "=" + o.FSType
	if strings.TrimSpace(base) == "" {
		return suffix
	}
	return base + " " + suffix
}

// ParseOverlayUpper extracts the overlay-upper descriptor from a kernel
// command line, ignoring unrelated tokens. An absent or partial descriptor
// yields an invalid (or empty) result, and the guest falls back to a tmpfs
// upper. Refs: NFR-17.7, MGIT-11.6.7
func ParseOverlayUpper(cmdline string) OverlayUpper {
	var o OverlayUpper
	for _, field := range strings.Fields(cmdline) {
		key, value, ok := strings.Cut(field, "=")
		if !ok || value == "" {
			continue
		}
		switch key {
		case KeyOverlayDev:
			o.Device = value
		case KeyOverlayFS:
			o.FSType = value
		}
	}
	return o
}

// ParseWorktreeMount extracts the worktree descriptor from a kernel
// command line. Unknown tokens are ignored (the cmdline also carries
// kernel/boot args); a key with no value is skipped. The guest calls this
// on /proc/cmdline. Refs: FR-17.3, MGIT-11.6.5
func ParseWorktreeMount(cmdline string) WorktreeMount {
	var w WorktreeMount
	for _, field := range strings.Fields(cmdline) {
		key, value, ok := strings.Cut(field, "=")
		if !ok || value == "" {
			continue
		}
		switch key {
		case KeyWorktreePath:
			w.Path = value
		case KeyWorktreeFS:
			w.FSType = value
		case KeyWorktreeSource:
			w.Source = value
		}
	}
	return w
}
