package model

import (
	"context"
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

// ExecRequest is one whole command routed into the guest over vsock.
// Env entries are explicit per-exec injections only — the host
// environment is never passed through (FR-17.3, FR-17.17).
// Refs: FR-17.11
type ExecRequest struct {
	Command []string      `json:"command"`           // argv; whole-command routing, no per-binary shimming
	Dir     string        `json:"dir,omitempty"`     // cwd inside the guest (identical-path mount)
	Env     []string      `json:"env,omitempty"`     // explicit injections, flagged in audit
	Timeout time.Duration `json:"timeout,omitempty"` // zero means the sandbox TTL governs
}

// Validate checks the exec request shape. Refs: FR-17.11
func (r ExecRequest) Validate() error {
	if len(r.Command) == 0 {
		return &ValidationError{Field: "command", Message: "must contain at least one argument"}
	}
	if r.Timeout < 0 {
		return &ValidationError{Field: "timeout", Message: "must be non-negative (zero = sandbox TTL governs)"}
	}
	return nil
}

// ExecResult carries the guest command outcome back unchanged.
// Refs: FR-17.11
type ExecResult struct {
	Stdout   []byte `json:"stdout"`
	Stderr   []byte `json:"stderr"`
	ExitCode int    `json:"exit_code"`
}

// SandboxManager abstracts microVM lifecycle per platform backend.
// Mirrors WorktreeManager (ADR-004); backends live in mgit-sandboxd.
// Refs: FR-17.15, FR-17.16, ADR-005
type SandboxManager interface {
	Launch(ctx context.Context, opts SandboxLaunchOptions) (*SandboxInfo, error)
	List(ctx context.Context) ([]SandboxInfo, error)
	Exec(ctx context.Context, id string, req ExecRequest) (*ExecResult, error)
	Stop(ctx context.Context, id string, force bool) error
	Remove(ctx context.Context, id string, force bool) error
	Resolve(ctx context.Context, id string) (*SandboxInfo, error)
}

// Attestation is a host-issued binding of one commit to the sandbox
// that produced it, recorded at land time. Both hashes of the ADR-002
// dual-hash model are bound. Refs: FR-17.6, FR-17.38
type Attestation struct {
	SandboxID     string    `json:"sandbox_id"`
	CommitHash    string    `json:"commit_hash"`    // git SHA-1 object ID
	ContentHash   string    `json:"content_hash"`   // mgit SHA-256 (ADR-002)
	HostSignature []byte    `json:"host_signature"` // issued by mgit-sandboxd
	IssuedAt      time.Time `json:"issued_at"`      // host receive-time, UTC (SEC-11, FR-17.28)
}

// Attestor issues and verifies commit attestations. HOST-SIDE ONLY
// (SEC-01): attestations are host-issued by mgit-sandboxd as commit
// objects cross vsock, keyed by host-held material the guest never
// sees. Guest code (mgit-guest) MUST NOT implement this interface and
// holds no signing key — an attestation minted by the thing being
// attested would be forgeable and worthless. Refs: FR-17.6, FR-17.38
type Attestor interface {
	Attest(ctx context.Context, sandboxID, commitHash, contentHash string) (*Attestation, error)
	Verify(ctx context.Context, att *Attestation) error
}

// Boundary-crossing capabilities a sandbox may request. Refs: FR-17.12
const (
	// CapabilityEgress requests one additional egress destination.
	CapabilityEgress = "egress"
	// CapabilitySSHAgent requests host ssh-agent socket forwarding.
	CapabilitySSHAgent = "ssh_agent"
	// CapabilityOpenNetwork requests open-network mode (user-accepted risk).
	CapabilityOpenNetwork = "open_network"
	// CapabilityMount requests an additional read-only mount.
	CapabilityMount = "mount"
)

// CapabilityRequest is one boundary-crossing capability ask (extra
// egress, ssh-agent forwarding, open network, additional mount). It is
// derived solely from the host-observed denied connection — never from
// guest-supplied text (SEC-05) — so the grant prompt always shows the
// real destination and requesting task. Refs: FR-17.12
type CapabilityRequest struct {
	SandboxID        string `json:"sandbox_id"`
	TaskID           string `json:"task_id"`
	Capability       string `json:"capability"` // CapabilityEgress | CapabilitySSHAgent | CapabilityOpenNetwork | CapabilityMount
	ObservedDestIP   string `json:"observed_dest_ip,omitempty"`
	ObservedDestPort int    `json:"observed_dest_port,omitempty"`
}

// Validate checks the capability request shape. Refs: FR-17.12
func (c CapabilityRequest) Validate() error {
	if c.SandboxID == "" {
		return &ValidationError{Field: "sandbox_id", Message: "must not be empty"}
	}
	switch c.Capability {
	case CapabilityEgress, CapabilitySSHAgent, CapabilityOpenNetwork, CapabilityMount:
		return nil
	default:
		return &ValidationError{Field: "capability", Message: fmt.Sprintf("unknown capability %q", c.Capability)}
	}
}
