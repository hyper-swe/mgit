package model

import (
	"fmt"
	"regexp"
	"time"
)

// Network policy modes for a sandbox, enforced host-side (the guest
// cannot weaken them). Refs: FR-17.7
const (
	// NetworkModeNone attaches no NIC; vsock control plane only.
	NetworkModeNone = "none"
	// NetworkModeAllowlist permits only flows to allowlisted resolved
	// IPs via the host egress proxy (the default). Refs: FR-17.8
	NetworkModeAllowlist = "allowlist"
	// NetworkModeOpen NATs to the host network; explicitly disables the
	// exfiltration/lateral-movement defenses. Never a default.
	NetworkModeOpen = "open"
)

// digestImageRefRe matches a digest-pinned image reference per
// FR-17.17: <name>@sha256:<64 lowercase hex>.
var digestImageRefRe = regexp.MustCompile(`^[a-z0-9][a-zA-Z0-9._/-]*@sha256:[a-f0-9]{64}$`)

// NetworkPolicy declares a sandbox's network posture at launch. It is
// recorded immutably in the audit record and enforced on the host.
// Refs: FR-17.7, FR-17.8
type NetworkPolicy struct {
	Mode      string   `json:"mode"`                // none | allowlist | open
	Allowlist []string `json:"allowlist,omitempty"` // host patterns, allowlist mode only
}

// Validate checks the network policy shape.
// Refs: FR-17.7
func (p NetworkPolicy) Validate() error {
	switch p.Mode {
	case NetworkModeNone, NetworkModeOpen:
		if len(p.Allowlist) > 0 {
			return &ValidationError{Field: "allowlist", Message: fmt.Sprintf("must be empty in %q mode", p.Mode)}
		}
	case NetworkModeAllowlist:
		// Allowlist may be empty: the host policy store supplies defaults
		// (registry hosts) per FR-17.13.
	default:
		return &ValidationError{Field: "mode", Message: fmt.Sprintf("unknown network mode %q", p.Mode)}
	}
	return nil
}

// SandboxLaunchOptions holds the parameters to provision a microVM
// bound to one task and one worktree. Refs: FR-17.1, FR-17.15, NFR-17.5
type SandboxLaunchOptions struct {
	TaskID       string        `json:"task_id"`
	WorktreePath string        `json:"worktree_path"`
	ImageRef     string        `json:"image_ref"` // pinned by digest (FR-17.17)
	Network      NetworkPolicy `json:"network"`
	CPUs         int           `json:"cpus"`
	MemoryMB     int           `json:"memory_mb"`
	DiskQuotaMB  int           `json:"disk_quota_mb"`
	TTL          time.Duration `json:"ttl"`
}

// Validate checks launch options: task binding, worktree path,
// digest-pinned image, network policy, and non-negative resources.
// Refs: FR-17.1, FR-17.7, FR-17.17, NFR-17.5
func (o SandboxLaunchOptions) Validate() error {
	if o.TaskID == "" {
		return &ValidationError{Field: "task_id", Message: "must not be empty"}
	}
	if _, err := ParseTaskID(o.TaskID); err != nil {
		return &ValidationError{Field: "task_id", Message: fmt.Sprintf("invalid format: %s", o.TaskID)}
	}
	if o.WorktreePath == "" {
		return &ValidationError{Field: "worktree_path", Message: "must not be empty"}
	}
	if !digestImageRefRe.MatchString(o.ImageRef) {
		return &ValidationError{Field: "image_ref", Message: "must be digest-pinned (<name>@sha256:<64 hex>)"}
	}
	if err := o.Network.Validate(); err != nil {
		return err
	}
	if o.CPUs < 0 || o.MemoryMB < 0 || o.DiskQuotaMB < 0 || o.TTL < 0 {
		return &ValidationError{Field: "resources", Message: "cpus, memory_mb, disk_quota_mb, and ttl must be non-negative"}
	}
	return nil
}

// SandboxInfo describes one registered sandbox. State is derived from
// the latest sandbox_events row, never stored mutably. Wraps backend
// types — no VMM or go-git types are exposed. Refs: FR-17.1, FR-17.18
type SandboxInfo struct {
	ID           string    `json:"id"`      // ULID
	TaskID       string    `json:"task_id"` // bound task (FR-17.1)
	WorktreePath string    `json:"worktree_path"`
	Backend      string    `json:"backend"`      // kvm | vzf | hyperv | container
	ImageDigest  string    `json:"image_digest"` // sha256 of rootfs image
	NetworkMode  string    `json:"network_mode"` // none | allowlist | open
	State        string    `json:"state"`        // derived: created | running | suspended | landed | destroyed
	CreatedAt    time.Time `json:"created_at"`   // ISO-8601 UTC
}

// Validate checks that the SandboxInfo has required fields.
// Refs: FR-17.1, FR-17.7
func (s SandboxInfo) Validate() error {
	if s.ID == "" {
		return &ValidationError{Field: "id", Message: "must not be empty"}
	}
	if s.TaskID == "" {
		return &ValidationError{Field: "task_id", Message: "must not be empty"}
	}
	return NetworkPolicy{Mode: s.NetworkMode}.Validate()
}
