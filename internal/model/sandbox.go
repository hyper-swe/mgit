package model

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"strings"
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

// Sandbox backends per platform. Refs: FR-17.15
const (
	// BackendKVM is the Linux KVM microVM backend.
	BackendKVM = "kvm"
	// BackendVZF is the macOS Virtualization.framework backend.
	BackendVZF = "vzf"
	// BackendHyperV is the Windows Hyper-V/WHP backend.
	BackendHyperV = "hyperv"
	// BackendContainer is the reduced-isolation fallback, permitted only
	// with explicit acknowledgment recorded in the audit trail.
	BackendContainer = "container"
)

// Sandbox lifecycle states, derived from the latest sandbox_events row
// (FR-17.18); never stored mutably. Refs: FR-17.9, FR-17.18
const (
	// StateCreated means registered; the VM may not have booted yet
	// (lazy provisioning, FR-17.10).
	StateCreated = "created"
	// StateRunning means the VM is booted and accepting exec requests.
	StateRunning = "running"
	// StateSuspended means the VM is paused by idle suspend (NFR-17.3).
	StateSuspended = "suspended"
	// StateLanded means commits were verified and imported (FR-17.5).
	StateLanded = "landed"
	// StateDestroyed means the sandbox was torn down.
	StateDestroyed = "destroyed"
)

// validBackends and validStates close the vocabularies above so writers
// of the append-only audit trail cannot fork them with typos.
var (
	validBackends = map[string]bool{BackendKVM: true, BackendVZF: true, BackendHyperV: true, BackendContainer: true}
	validStates   = map[string]bool{StateCreated: true, StateRunning: true, StateSuspended: true, StateLanded: true, StateDestroyed: true}
)

// Image-reference grammar per FR-17.17: lowercase OCI-style name
// components (the first may carry a registry :port), pinned by a
// sha256 digest. Tag-only and mixed-case references are rejected.
var (
	imageNameComponentRe = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*$`)
	registryPortRe       = regexp.MustCompile(`^[0-9]+$`)
	sha256DigestRe       = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	// allowlistEntryRe matches one egress allowlist entry: lowercase
	// hostname (optionally wildcarded), IP, host:port, or CIDR. Control
	// characters and uppercase are rejected — entries are written
	// verbatim into the append-only audit record (F-09).
	allowlistEntryRe = regexp.MustCompile(`^[a-z0-9*][a-z0-9.*:-]{0,252}(?:/[0-9]{1,3})?$`)
	// hexHashRe matches a bare lowercase hex hash of the given length.
	sha1HexRe   = regexp.MustCompile(`^[a-f0-9]{40}$`)
	sha256HexRe = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

// maxImageRefLen bounds image references before any parsing work (OCI
// caps repository names well below this; oversized refs are hostile).
const maxImageRefLen = 512

// ValidateImageRef checks that an image reference is digest-pinned per
// FR-17.17: <name>@sha256:<64 hex>, where <name> is one or more
// lowercase components separated by '/', and the first component may
// carry a registry port (host:5000/...). Exported so images.lock
// handling (FR-17.31, FR-17.36) validates with the same grammar.
func ValidateImageRef(ref string) error {
	if len(ref) > maxImageRefLen {
		return &ValidationError{Field: "image_ref", Message: fmt.Sprintf("exceeds %d bytes", maxImageRefLen)}
	}
	name, digest, found := strings.Cut(ref, "@")
	if !found || !sha256DigestRe.MatchString(digest) {
		return &ValidationError{Field: "image_ref", Message: "must be digest-pinned (<name>@sha256:<64 hex>)"}
	}
	for i, component := range strings.Split(name, "/") {
		host, port, hasPort := strings.Cut(component, ":")
		if hasPort && (i != 0 || !registryPortRe.MatchString(port)) {
			return &ValidationError{Field: "image_ref", Message: fmt.Sprintf("invalid name component %q", component)}
		}
		if !imageNameComponentRe.MatchString(host) {
			return &ValidationError{Field: "image_ref", Message: fmt.Sprintf("invalid name component %q", component)}
		}
	}
	return nil
}

// NetworkOpenRiskNote is the risk recorded whenever a sandbox is launched
// in open-network mode. Open NATs to the host network and therefore
// explicitly disables the exfiltration (T3) and lateral-movement (T9)
// defenses — a user-accepted risk, never an auto-selected default.
// Refs: FR-17.7, ADR-005 (threats T3, T9)
const NetworkOpenRiskNote = "open network NATs to the host network; the T3 (exfiltration) and T9 (lateral-movement) defenses are disabled — user-accepted risk, never a default"

// NetworkRiskNote reports the audit risk note for a network mode and
// whether that mode is a risk-bearing posture. Only open mode carries a
// note: none has no NIC and allowlist is host-proxy-confined, so neither
// weakens T3/T9. The note is recorded in the append-only created event so
// every open-mode sandbox is permanently attributable. Refs: FR-17.7
func NetworkRiskNote(mode string) (string, bool) {
	if mode == NetworkModeOpen {
		return NetworkOpenRiskNote, true
	}
	return "", false
}

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
		// (registry hosts) per FR-17.13. Entries are audit-record bytes
		// (F-09): enforce the grammar at the validation boundary.
		for _, entry := range p.Allowlist {
			if !allowlistEntryRe.MatchString(entry) {
				return &ValidationError{Field: "allowlist", Message: fmt.Sprintf("invalid entry %q", entry)}
			}
		}
	default:
		return &ValidationError{Field: "mode", Message: fmt.Sprintf("unknown network mode %q", p.Mode)}
	}
	return nil
}

// SandboxLaunchOptions holds the parameters to provision a microVM
// bound to one task and one worktree. Zero resource values mean "use
// the host policy store default" (FR-17.13, NFR-17.5).
// Refs: FR-17.1, FR-17.15
type SandboxLaunchOptions struct {
	// SandboxID, when set, is the host-assigned lifecycle ID the backend
	// must use for this VM (so a sandbox registered lazily and booted
	// later share one ID, FR-17.10). Empty means the backend generates
	// one — the legacy/direct path.
	SandboxID    string        `json:"sandbox_id,omitempty"`
	TaskID       string        `json:"task_id"`
	WorktreePath string        `json:"worktree_path"`
	ImageRef     string        `json:"image_ref"` // pinned by digest (FR-17.17)
	Network      NetworkPolicy `json:"network"`
	CPUs         int           `json:"cpus,omitempty"`          // 0 = policy default
	MemoryMB     int           `json:"memory_mb,omitempty"`     // 0 = policy default
	DiskQuotaMB  int           `json:"disk_quota_mb,omitempty"` // 0 = policy default
	TTL          time.Duration `json:"ttl_ns,omitempty"`        // nanoseconds; 0 = policy default
	// ConfineAgent opts this sandbox into the T2 fully-confined-agent
	// topology (ADR-005, MGIT-11.11.4). Defaults false (T1). Carries NO
	// credential material — secrets are injected per session, never baked
	// into the launch/image config (the no-credentials-in-image guarantee).
	ConfineAgent bool `json:"confine_agent,omitempty"`
}

// Validate checks launch options: task binding, worktree path,
// digest-pinned image, network policy, and non-negative resources.
// Refs: FR-17.1, FR-17.7, FR-17.17, NFR-17.5
func (o SandboxLaunchOptions) Validate() error {
	if err := validateTaskIDField(o.TaskID); err != nil {
		return err
	}
	if o.WorktreePath == "" {
		return &ValidationError{Field: "worktree_path", Message: "must not be empty"}
	}
	if err := ValidateImageRef(o.ImageRef); err != nil {
		return err
	}
	if err := o.Network.Validate(); err != nil {
		return nestField("network", err)
	}
	for field, value := range map[string]int64{
		"cpus": int64(o.CPUs), "memory_mb": int64(o.MemoryMB),
		"disk_quota_mb": int64(o.DiskQuotaMB), "ttl_ns": int64(o.TTL),
	} {
		if value < 0 {
			return &ValidationError{Field: field, Message: "must be non-negative (zero = policy default)"}
		}
	}
	return nil
}

// nestField prefixes a nested struct's ValidationError field with its
// parent JSON field name so callers can locate the offending input.
func nestField(parent string, err error) error {
	var vErr *ValidationError
	if errors.As(err, &vErr) {
		return &ValidationError{Field: parent + "." + vErr.Field, Message: vErr.Message}
	}
	return err
}

// SandboxInfo describes one registered sandbox. State is derived from
// the latest sandbox_events row, never stored mutably. NetworkMode and
// NetworkAllowlist mirror the immutable launch-time policy so audits
// can read the egress posture from List/Resolve (FR-17.7). ExpiresAt
// (launch time + TTL) lets service-level prune reap expired sandboxes
// via List+Remove (FR-17.9). Wraps backend types — no VMM or go-git
// types are exposed. Refs: FR-17.1, FR-17.18
type SandboxInfo struct {
	ID               string    `json:"id"`      // ULID
	TaskID           string    `json:"task_id"` // bound task (FR-17.1)
	WorktreePath     string    `json:"worktree_path"`
	Backend          string    `json:"backend"`                     // BackendKVM | BackendVZF | BackendHyperV | BackendContainer
	ImageDigest      string    `json:"image_digest"`                // sha256 of rootfs image
	NetworkMode      string    `json:"network_mode"`                // NetworkModeNone | NetworkModeAllowlist | NetworkModeOpen
	NetworkAllowlist []string  `json:"network_allowlist,omitempty"` // immutable launch-time allowlist (FR-17.7)
	State            string    `json:"state"`                       // derived (StateCreated..StateDestroyed)
	MemoryMB         int       `json:"memory_mb,omitempty"`         // effective memory cap; feeds the FR-17.26 ceiling
	CreatedAt        time.Time `json:"created_at"`                  // ISO-8601 UTC
	ExpiresAt        time.Time `json:"expires_at,omitempty"`        // TTL deadline; zero = no TTL
}

// Validate checks that the SandboxInfo has required, well-formed
// fields and a closed-vocabulary backend/state. An empty State is
// permitted (state is derived from sandbox_events after registration);
// an empty or malformed ImageDigest is not — the digest is the
// FR-17.17 pinning guarantee in the audit record.
// Refs: FR-17.1, FR-17.7, FR-17.15, FR-17.17
func (s SandboxInfo) Validate() error {
	if s.ID == "" {
		return &ValidationError{Field: "id", Message: "must not be empty"}
	}
	if err := validateTaskIDField(s.TaskID); err != nil {
		return err
	}
	if s.WorktreePath == "" {
		return &ValidationError{Field: "worktree_path", Message: "must not be empty"}
	}
	if !validBackends[s.Backend] {
		return &ValidationError{Field: "backend", Message: fmt.Sprintf("unknown backend %q", s.Backend)}
	}
	if !sha256DigestRe.MatchString(s.ImageDigest) {
		return &ValidationError{Field: "image_digest", Message: "must be sha256:<64 hex>"}
	}
	if s.State != "" && !validStates[s.State] {
		return &ValidationError{Field: "state", Message: fmt.Sprintf("unknown state %q", s.State)}
	}
	return nestField("network", NetworkPolicy{Mode: s.NetworkMode, Allowlist: s.NetworkAllowlist}.Validate())
}

// ExecRequest is one whole command routed into the guest over vsock.
// Env entries are explicit per-exec injections only — the host
// environment is never passed through (FR-17.3, FR-17.17).
// Refs: FR-17.11
type ExecRequest struct {
	Command []string      `json:"command"`              // argv; whole-command routing, no per-binary shimming
	Dir     string        `json:"dir,omitempty"`        // cwd inside the guest (identical-path mount)
	Env     []string      `json:"env,omitempty"`        // explicit injections, flagged in audit
	Timeout time.Duration `json:"timeout_ns,omitempty"` // nanoseconds; zero means the sandbox TTL governs
}

// Validate checks the exec request shape. Refs: FR-17.11
func (r ExecRequest) Validate() error {
	if len(r.Command) == 0 {
		return &ValidationError{Field: "command", Message: "must contain at least one argument"}
	}
	if r.Timeout < 0 {
		return &ValidationError{Field: "timeout_ns", Message: "must be non-negative (zero = sandbox TTL governs)"}
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
// The verb set is fixed by FR-17.15; prune (FR-17.2, FR-17.9) is a
// service-level operation built on List (State/ExpiresAt) + Remove,
// not a backend verb. Refs: FR-17.15, FR-17.16, ADR-005
type SandboxManager interface {
	Launch(ctx context.Context, opts SandboxLaunchOptions) (*SandboxInfo, error)
	List(ctx context.Context) ([]SandboxInfo, error)
	Exec(ctx context.Context, id string, req ExecRequest) (*ExecResult, error)
	Stop(ctx context.Context, id string, force bool) error
	Remove(ctx context.Context, id string, force bool) error
	Resolve(ctx context.Context, id string) (*SandboxInfo, error)
}

// AlgEd25519 is the v1 attestation signature algorithm (FR-17.29,
// stdlib crypto/ed25519 per the APPROVED-PACKAGES §2a decision).
const AlgEd25519 = "ed25519"

// Attestation is a host-issued binding of one commit to the sandbox
// that produced it, recorded at land time. Both hashes of the ADR-002
// dual-hash model are bound. Alg and KeyID make the signature
// verifiable across key rotations (FR-17.38) and rule out
// algorithm-confusion. Refs: FR-17.6, FR-17.38
type Attestation struct {
	SandboxID     string    `json:"sandbox_id"`
	CommitHash    string    `json:"commit_hash"`    // git SHA-1 object ID (40 hex)
	ContentHash   string    `json:"content_hash"`   // mgit SHA-256, 64 hex (ADR-002)
	Alg           string    `json:"alg"`            // signature algorithm (AlgEd25519)
	KeyID         string    `json:"key_id"`         // host trust-anchor fingerprint (FR-17.38)
	HostSignature []byte    `json:"host_signature"` // issued by mgit-sandboxd
	IssuedAt      time.Time `json:"issued_at"`      // host receive-time, UTC (SEC-11, FR-17.28)
}

// Validate checks the attestation shape. Signature *verification* is
// the Attestor's job; this rejects structurally hollow attestations
// before they reach a verifier. Refs: FR-17.6
func (a Attestation) Validate() error {
	if a.SandboxID == "" {
		return &ValidationError{Field: "sandbox_id", Message: "must not be empty"}
	}
	if !sha1HexRe.MatchString(a.CommitHash) {
		return &ValidationError{Field: "commit_hash", Message: "must be 40 lowercase hex (git SHA-1)"}
	}
	if !sha256HexRe.MatchString(a.ContentHash) {
		return &ValidationError{Field: "content_hash", Message: "must be 64 lowercase hex (SHA-256)"}
	}
	if a.Alg == "" || a.KeyID == "" {
		return &ValidationError{Field: "alg", Message: "alg and key_id must identify the signing scheme"}
	}
	if len(a.HostSignature) == 0 {
		return &ValidationError{Field: "host_signature", Message: "must not be empty"}
	}
	if a.IssuedAt.IsZero() {
		return &ValidationError{Field: "issued_at", Message: "must carry the host receive-time"}
	}
	return nil
}

// Attestor issues and verifies commit attestations. HOST-SIDE ONLY
// (SEC-01): attestations are host-issued by mgit-sandboxd as commit
// objects cross vsock, keyed by host-held material the guest never
// sees. Guest code (mgit-guest) MUST NOT implement this interface and
// holds no signing key — an attestation minted by the thing being
// attested would be forgeable and worthless. Refs: FR-17.6, FR-17.38
type Attestor interface {
	// Attest issues an attestation for one commit. Implementations MUST
	// refuse any (sandboxID, hash) pair the daemon did not itself
	// observe crossing that sandbox's vsock channel — Attest is not an
	// attest-anything signing oracle (SEC-01).
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

// Validate checks the capability request shape: the requesting sandbox
// and task must be identified (the grant prompt shows the task), and
// egress requests must carry the host-observed destination — the whole
// point of SEC-05. Refs: FR-17.12
func (c CapabilityRequest) Validate() error {
	if c.SandboxID == "" {
		return &ValidationError{Field: "sandbox_id", Message: "must not be empty"}
	}
	if err := validateTaskIDField(c.TaskID); err != nil {
		return err
	}
	switch c.Capability {
	case CapabilityEgress:
		// The destination must parse as a real IP address: this string
		// is rendered in the human grant prompt and written to the
		// audit record — free text here IS the SEC-05 attack.
		if _, err := netip.ParseAddr(c.ObservedDestIP); err != nil {
			return &ValidationError{Field: "observed_dest_ip", Message: "must be the host-observed destination IP address (SEC-05)"}
		}
		if c.ObservedDestPort < 1 || c.ObservedDestPort > 65535 {
			return &ValidationError{Field: "observed_dest_port", Message: "must be a valid port (1-65535)"}
		}
	case CapabilitySSHAgent, CapabilityOpenNetwork, CapabilityMount:
		// No observed destination required.
	default:
		return &ValidationError{Field: "capability", Message: fmt.Sprintf("unknown capability %q", c.Capability)}
	}
	return nil
}

// ObservedDenial carries ONLY the facts the HOST observed when it refused a
// guest flow: the host-resolved/pinned destination IP, the destination port,
// and the (sandbox, task) the flow came from. It is the single, structurally
// constrained input a capability-escalation request may be derived from
// (SEC-05). There is deliberately NO field for a guest-supplied hostname,
// "remedy text", or free-form reason: the grant prompt and audit record must
// show the real host-observed destination, never anything the guest can
// dictate. Construct it from the host egress engine's Decision, never from a
// guest CONNECT frame. Refs: FR-17.12, SEC-05
type ObservedDenial struct {
	SandboxID string     // host-owned sandbox lifecycle ID
	TaskID    string     // bound task (FR-17.1); the grant prompt shows it
	DestIP    netip.Addr // host-resolved/pinned destination (never guest text)
	DestPort  int        // host-observed destination port
	Rule      string     // host-authored deny reason (for the audit record)
}

// RequestFromObservedDenial builds an egress CapabilityRequest from the
// HOST-observed denial alone. It is the SEC-05 chokepoint: the only fields
// that reach the request (and therefore the grant prompt and the append-only
// audit record) are the host's own destination IP, port, sandbox, and task.
// A guest cannot supply or influence any of them — the function takes no
// guest string at all. A denial with an invalid host-observed IP/port yields
// a validation error rather than a forgeable request. Refs: FR-17.12, SEC-05
func (d ObservedDenial) RequestFromObservedDenial() (CapabilityRequest, error) {
	req := CapabilityRequest{
		SandboxID:        d.SandboxID,
		TaskID:           d.TaskID,
		Capability:       CapabilityEgress,
		ObservedDestIP:   d.DestIP.String(),
		ObservedDestPort: d.DestPort,
	}
	if err := req.Validate(); err != nil {
		return CapabilityRequest{}, fmt.Errorf("capability request from observed denial: %w", err)
	}
	return req, nil
}

// GrantScopeSandboxLifetime is the only grant scope: a capability grant lives
// exactly as long as the sandbox it was granted to. There is no "permanent"
// or "all sandboxes" scope, and no allow-all capability — each grant names one
// concrete destination and dies on teardown. Refs: FR-17.12, SEC-05
const GrantScopeSandboxLifetime = "sandbox_lifetime"

// CapabilityGrant is a recorded, sandbox-lifetime-scoped approval of one
// CapabilityRequest. It is append-only audited (as a policy_granted sandbox
// event) and held live only while the sandbox runs; teardown drops it. There
// is no allow-all: a grant always names the one host-observed destination it
// authorizes. Refs: FR-17.12, FR-17.18, SEC-05
type CapabilityGrant struct {
	SandboxID        string    `json:"sandbox_id"`
	TaskID           string    `json:"task_id"`
	Capability       string    `json:"capability"`
	ObservedDestIP   string    `json:"observed_dest_ip,omitempty"`
	ObservedDestPort int       `json:"observed_dest_port,omitempty"`
	Scope            string    `json:"scope"`      // always GrantScopeSandboxLifetime
	GrantedAt        time.Time `json:"granted_at"` // host clock, UTC
}

// AllowlistEntry renders an egress grant as a host:port allowlist entry the
// live egress engine can admit (the grant widens the running sandbox's
// allowlist, never the persisted launch policy). It is meaningful only for
// CapabilityEgress grants. Refs: FR-17.12
func (g CapabilityGrant) AllowlistEntry() string {
	return fmt.Sprintf("%s:%d", g.ObservedDestIP, g.ObservedDestPort)
}
