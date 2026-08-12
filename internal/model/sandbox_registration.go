package model

import "time"

// SandboxRegistration is the DURABLE record of one registered sandbox: what a
// fresh daemon needs to bring a registration back after the daemon that
// created it has exited.
//
// It exists because the live registry used to be daemon-process memory alone.
// Registration is LAZY (FR-17.9, FR-17.10) — `mgit work --sandbox` books a
// task+worktree binding WITHOUT booting a VM — so a never-used sandbox held no
// VM keeping anything alive, its daemon idle-exited (NFR-17.6), and the
// containment an agent had been told it had silently ceased to exist. Losing
// containment availability that way turns a safety property into a routing
// decision made by an agent under progress pressure (MGIT-102).
//
// Info is the reportable view (identity, resolved caps, derived state).
// ImageRef, TTL and ConfineAgent are the launch inputs SandboxInfo does not
// carry, so a boot that has not happened yet can still happen with exactly the
// options the sandbox was registered with.
// Refs: FR-17.1, FR-17.9, FR-17.10, FR-17.18, MGIT-102
type SandboxRegistration struct {
	Info         SandboxInfo   `json:"info"`
	ImageRef     string        `json:"image_ref"`        // digest-pinned (FR-17.17)
	TTL          time.Duration `json:"ttl_ns,omitempty"` // effective TTL; 0 = policy default
	ConfineAgent bool          `json:"confine_agent,omitempty"`
}

// LaunchOptions rebuilds the launch options the sandbox was registered with so
// a rehydrated registration boots exactly as the original would have — same
// host-assigned ID, same pinned image, same egress policy, same resolved caps.
// Refs: FR-17.10, MGIT-102
func (r SandboxRegistration) LaunchOptions() SandboxLaunchOptions {
	return SandboxLaunchOptions{
		SandboxID:    r.Info.ID,
		TaskID:       r.Info.TaskID,
		WorktreePath: r.Info.WorktreePath,
		ImageRef:     r.ImageRef,
		Network:      NetworkPolicy{Mode: r.Info.NetworkMode, Allowlist: r.Info.NetworkAllowlist},
		CPUs:         r.Info.CPUs,
		MemoryMB:     r.Info.MemoryMB,
		DiskQuotaMB:  r.Info.DiskQuotaMB,
		TTL:          r.TTL,
		ConfineAgent: r.ConfineAgent,
		PublishPorts: r.Info.PublishPorts,
	}
}

// Validate rejects a registration that could not be brought back usefully. A
// row in the durable registry is a claim that a sandbox EXISTS, so it must
// carry the identity, the pinned image and the egress posture a later boot
// needs; a hollow row would resurrect a sandbox nothing can launch, and a row
// with a lost allowlist would resurrect one with the wrong containment.
//
// It deliberately does NOT reuse SandboxInfo.Validate, which requires a
// backend: a lazily registered sandbox has not booted, so no backend has been
// chosen yet, and demanding one would make the never-booted case — the one
// this registry exists for — unstorable. An empty state, by contrast, IS
// rejected: the registry's whole job is to say what state was last observed.
// Refs: FR-17.1, FR-17.7, FR-17.17, MGIT-102
func (r SandboxRegistration) Validate() error {
	if r.Info.ID == "" {
		return &ValidationError{Field: "id", Message: "must not be empty"}
	}
	if err := validateTaskIDField(r.Info.TaskID); err != nil {
		return err
	}
	if r.Info.WorktreePath == "" {
		return &ValidationError{Field: "worktree_path", Message: "must not be empty"}
	}
	if err := ValidateImageRef(r.ImageRef); err != nil {
		return err
	}
	if r.Info.Backend != "" && !validBackends[r.Info.Backend] {
		return &ValidationError{Field: "backend", Message: "unknown backend " + r.Info.Backend}
	}
	if !ValidSandboxState(r.Info.State) {
		return &ValidationError{Field: "state", Message: "must be a known sandbox state, got " + r.Info.State}
	}
	if r.TTL < 0 {
		return &ValidationError{Field: "ttl_ns", Message: "must be non-negative (zero = policy default)"}
	}
	if err := validatePublishPorts(r.Info.PublishPorts); err != nil {
		return nestField("publish_ports", err)
	}
	return nestField("network", NetworkPolicy{Mode: r.Info.NetworkMode, Allowlist: r.Info.NetworkAllowlist}.Validate())
}
