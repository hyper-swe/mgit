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
// posture (SEC-01).
//
// TWO TRANSPORTS, ONE FORMAT. libkrun boots libkrunfw's OWN kernel, so that
// backend never composes a command line and has nowhere to append these
// tokens. It supplies the identical tokens through the guest ENVIRONMENT
// instead (EnvBootTokens), and the guest merges both sources with BootTokens
// before parsing — so every descriptor parser here is reused verbatim rather
// than growing a second format per backend. Refs: FR-17.3, MGIT-11.6.5, ADR-010
package guestboot

import (
	"path/filepath"
	"strconv"
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
	// KeyPublishPorts is the comma-separated list of GUEST TCP ports the guest
	// must expose for one-way host->guest port publishing (SEC-09). For each
	// listed port N the guest runs an AF_VSOCK(:N)->TCP(127.0.0.1:N) bridge so
	// the host publisher's host->guest vsock connect reaches the guest's own
	// loopback dev server. Empty/absent means no ports are published. The
	// cmdline carries only port numbers — no secrets, no host addresses — so
	// it cannot give the guest a path to the host (SEC-09 stays one-way).
	KeyPublishPorts = "mgit.publish_ports"
)

// EnvBootTokens is the guest environment variable carrying the same
// space-separated boot tokens the kernel command line carries, for backends
// that cannot compose a cmdline (libkrun boots libkrunfw's own kernel). It is
// host-supplied — the backend sets it when configuring the VM — and reaches
// the guest exactly like the cmdline does.
//
// It carries boot descriptors — a mount path, a tag, port numbers — and never
// a secret; SEC-01 keeps those off both transports. Refs: FR-17.3, ADR-010
//
//nolint:gosec // G101 false positive: this is an env-var NAME, not a credential.
const EnvBootTokens = "MGIT_GUEST_BOOT"

// BootTokens merges the two host->guest token transports into the single
// string the parsers below consume.
//
// The cmdline goes FIRST and the env LAST: the parsers overwrite as they
// scan, so the last occurrence of a key wins, which makes the env channel
// authoritative wherever both carry the same key.
//
// That ordering is the opposite of what cmdline convention suggests, and it
// is deliberate. libkrun renders the workload's whole ENVIRONMENT onto
// /proc/cmdline in its own syntax, wrapping each variable in double quotes —
// so a guest reading the cmdline sees a quote-mangled copy of the very
// descriptor mgit put in the environment, ending in mgit.worktree_src=work"
// rather than work. Letting the cmdline win meant the guest tried to mount a
// virtiofs tag that did not exist and died at boot with a bare EINVAL.
//
// A backend that genuinely owns the cmdline (firecracker, vzf) is unaffected:
// it sets no env channel, so there is nothing to override it.
//
// For the record: the strictly correct rule is "the channel THIS BACKEND
// owns wins" -- per-backend, not a single global precedence. Env-over-cmdline
// is only safe here because no shipped backend sets BOTH channels for the
// same VM (libkrun: env only; firecracker/vzf: cmdline only), so the two
// orderings coincide today. That coincidence is latent, not guaranteed: a
// backend that composed both would need this function to know which one it
// owns, which this signature cannot express. Re-verified empirically on
// firecracker 2026-07-30 (the live e2e suite, unchanged pass count) rather
// than by this reasoning alone. Refs: FR-17.3, ADR-010, MGIT-61.15
func BootTokens(cmdline, env string) string {
	switch {
	case strings.TrimSpace(cmdline) == "":
		return env
	case strings.TrimSpace(env) == "":
		return cmdline
	}
	return cmdline + " " + env
}

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

// AppendPublishPortsCmdline returns base with the published-ports descriptor
// appended as a single key=value token, the value being the guest ports
// joined by commas (e.g. "mgit.publish_ports=3000,8080"). An empty port list
// adds nothing (no ports published). Out-of-range ports (not 1..65535) are
// dropped: the descriptor only ever names valid guest TCP ports, so the guest
// never tries to listen on a bogus vsock port. Refs: SEC-09, FR-17.8
func AppendPublishPortsCmdline(base string, guestPorts []int) string {
	tokens := make([]string, 0, len(guestPorts))
	for _, p := range guestPorts {
		if p < 1 || p > 65535 {
			continue
		}
		tokens = append(tokens, strconv.Itoa(p))
	}
	if len(tokens) == 0 {
		return base
	}
	suffix := KeyPublishPorts + "=" + strings.Join(tokens, ",")
	if strings.TrimSpace(base) == "" {
		return suffix
	}
	return base + " " + suffix
}

// ParsePublishPorts extracts the guest ports to bridge from a kernel command
// line, ignoring unrelated tokens. Malformed or out-of-range entries are
// skipped (the guest only listens on valid ports); an absent descriptor
// yields an empty slice (no bridges). The guest calls this on /proc/cmdline.
// Refs: SEC-09, FR-17.8
func ParsePublishPorts(cmdline string) []int {
	var ports []int
	for _, field := range strings.Fields(cmdline) {
		key, value, ok := strings.Cut(field, "=")
		if !ok || key != KeyPublishPorts || value == "" {
			continue
		}
		for _, tok := range strings.Split(value, ",") {
			n, err := strconv.Atoi(strings.TrimSpace(tok))
			if err != nil || n < 1 || n > 65535 {
				continue
			}
			ports = append(ports, n)
		}
	}
	return ports
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
