package model

import (
	"fmt"
	"time"
)

// SandboxPolicy is the host-side enforcement configuration (SEC-02):
// it lives under the host config root, is never a committable repo
// file, and is never mounted into guests. Refs: FR-17.13, FR-17.6
type SandboxPolicy struct {
	// RequireSandbox makes land refuse unattested commits (FR-17.6).
	// Defaults to true; disable only by explicit host-side policy, and
	// the disablement is audited.
	RequireSandbox bool `json:"require_sandbox"`
	// Network is the default network posture for new sandboxes.
	Network NetworkPolicy `json:"network"`
	// ImageLockRef names the images.lock file under the host root
	// (FR-17.17, FR-17.36). Empty means <host-root>/images.lock.
	ImageLockRef string `json:"image_lock_ref,omitempty"`
	// SensitivePaths are host-trusted paths mounted read-only into
	// guests; land refuses guest modifications to them (FR-17.14).
	SensitivePaths []string `json:"sensitive_paths"`
	// Default resource caps (NFR-17.5); zero means backend minimums.
	CPUs        int           `json:"cpus"`
	MemoryMB    int           `json:"memory_mb"`
	DiskQuotaMB int           `json:"disk_quota_mb"`
	TTL         time.Duration `json:"ttl_ns"`
	// Per-sandbox maxima (R-H212): the largest a SINGLE launch may declare
	// for itself. These are a third, distinct thing from the two above them
	// and the two below:
	//   - the defaults say what an undeclared launch GETS,
	//   - these maxima say what a declaring launch may ASK FOR,
	//   - the aggregate ceilings say what the whole fleet may consume.
	// A request over one of these is REFUSED naming the limit, never
	// clamped: a caller that silently gets less than it asked for concludes
	// its workload is at fault and reshapes it (the R-H212 defect, where a
	// sandbox's memory ceiling nearly became permanent bundler config in a
	// customer's repository). Zero disables that dimension's per-sandbox
	// bound — the aggregate ceiling still applies. Refs: R-H212, NFR-17.5
	MaxCPUs        int `json:"max_cpus"`
	MaxMemoryMB    int `json:"max_memory_mb"`
	MaxDiskQuotaMB int `json:"max_disk_quota_mb"`
	// Global ceilings enforced by mgit-sandboxd across all sandboxes
	// (FR-17.26): exceeding either fails launch rather than degrading
	// the host.
	MaxConcurrentSandboxes int `json:"max_concurrent_sandboxes"`
	MaxTotalMemoryPercent  int `json:"max_total_memory_percent"`
	// ConfineAgent selects the T2 fully-confined-agent topology (ADR-005):
	// the guest bundles the agent CLI and the user attaches via
	// `mgit sandbox shell`. Strictly opt-in; defaults false (T1, the agent
	// runs on the host and commands are routed in). Refs: MGIT-11.11.4
	ConfineAgent bool `json:"confine_agent"`
}

// DefaultSandboxPolicy returns the safe defaults: require_sandbox on,
// allowlist networking, the FR-17.14 host-trusted path list, and the
// NFR-17.5 resource caps. Refs: FR-17.6, FR-17.13, FR-17.14, NFR-17.5
func DefaultSandboxPolicy() SandboxPolicy {
	return SandboxPolicy{
		RequireSandbox: true,
		Network:        NetworkPolicy{Mode: NetworkModeAllowlist},
		SensitivePaths: []string{
			".claude/", ".envrc", ".git/hooks/", ".vscode/",
			".cursor/", "AGENTS.md", "CLAUDE.md",
		},
		CPUs:        2,
		MemoryMB:    2048,
		DiskQuotaMB: 4096,
		TTL:         4 * time.Hour,
		// R-H212 per-sandbox maxima: generous enough that a real production
		// build (the motivating case peaked at 2.10 GB RSS) is declarable
		// WITHOUT an operator editing host policy — a caller editing the
		// operator's policy to fit its own workload defeats the point of
		// having one — yet bounded so a single launch cannot claim a whole
		// workstation. Operators raise or lower them in the host policy file.
		MaxCPUs:        8,
		MaxMemoryMB:    16384,
		MaxDiskQuotaMB: 65536,
		// FR-17.26 defaults: 8 concurrent sandboxes, 50% of host memory.
		MaxConcurrentSandboxes: 8,
		MaxTotalMemoryPercent:  50,
	}
}

// Validate checks the policy shape. An empty sensitive-path list is
// rejected: removing every host-trusted path (including via a
// repo-suggested "sensitive_paths": null) would silently disable the
// FR-17.14 config-injection defense. Refs: FR-17.13, FR-17.14
func (p SandboxPolicy) Validate() error {
	if err := p.Network.Validate(); err != nil {
		return nestField("network", err)
	}
	if len(p.SensitivePaths) == 0 {
		return &ValidationError{Field: "sensitive_paths", Message: "must protect at least one host-trusted path (FR-17.14)"}
	}
	for _, path := range p.SensitivePaths {
		if path == "" {
			return &ValidationError{Field: "sensitive_paths", Message: "entries must not be empty"}
		}
	}
	if p.MaxTotalMemoryPercent < 0 || p.MaxTotalMemoryPercent > 100 {
		return &ValidationError{Field: "max_total_memory_percent", Message: "must be 0-100"}
	}
	if p.MaxConcurrentSandboxes < 0 {
		return &ValidationError{Field: "max_concurrent_sandboxes", Message: "must be non-negative"}
	}
	for field, value := range map[string]int64{
		"cpus": int64(p.CPUs), "memory_mb": int64(p.MemoryMB),
		"disk_quota_mb": int64(p.DiskQuotaMB), "ttl_ns": int64(p.TTL),
	} {
		if value < 0 {
			return &ValidationError{Field: field, Message: fmt.Sprintf("must be non-negative, got %d", value)}
		}
	}
	return p.validateResourceMaxima()
}

// resourceDimension is one bounded resource: what a launch declares, what the
// policy defaults it to, and the policy field naming its per-sandbox maximum.
// Refs: R-H212
type resourceDimension struct {
	field     string // launch-option field name (the caller's vocabulary)
	maxField  string // host-policy field that sets the maximum
	flag      string // CLI flag that declares it
	unit      string // human unit for messages ("MB", "vCPU")
	requested int
	limit     int
	def       int
}

// dimensions enumerates the bounded resources for one declared request.
// Refs: R-H212, NFR-17.5
func (p SandboxPolicy) dimensions(o SandboxLaunchOptions) []resourceDimension {
	return []resourceDimension{
		{"cpus", "max_cpus", "--cpus", "vCPU", o.CPUs, p.MaxCPUs, p.CPUs},
		{"memory_mb", "max_memory_mb", "--memory-mb", "MB", o.MemoryMB, p.MaxMemoryMB, p.MemoryMB},
		{"disk_quota_mb", "max_disk_quota_mb", "--disk-quota-mb", "MB", o.DiskQuotaMB, p.MaxDiskQuotaMB, p.DiskQuotaMB},
	}
}

// validateResourceMaxima rejects negative maxima and any policy whose own
// default exceeds its own per-sandbox maximum — that policy would make every
// undeclared launch instantly illegal. Refs: R-H212, FR-17.13
func (p SandboxPolicy) validateResourceMaxima() error {
	for _, d := range p.dimensions(SandboxLaunchOptions{}) {
		if d.limit < 0 {
			return &ValidationError{Field: d.maxField, Message: fmt.Sprintf("must be non-negative, got %d", d.limit)}
		}
		if d.limit > 0 && d.def > d.limit {
			return &ValidationError{Field: d.maxField, Message: fmt.Sprintf(
				"per-sandbox maximum %d is below the default %s of %d", d.limit, d.field, d.def)}
		}
	}
	return nil
}

// EnforceResourceLimits refuses a launch that declares more of any resource
// than this policy's per-sandbox maximum. It is the "declared by the workload,
// bounded by the operator" half of R-H212, and it REFUSES rather than clamps
// on purpose: a caller handed 2 GB after asking for 4 concludes the ceiling is
// not the problem and goes back to reshaping its build — which is the defect
// this whole mechanism exists to prevent.
//
// A zero request means "policy default" and is always legal. A zero maximum
// disables that dimension's bound. This is the PER-SANDBOX check only: the
// FR-17.26 aggregate ceiling applies independently on top, and its refusal
// (ErrSandboxCeilingExceeded — the fleet is full) is a different problem with
// a different fix from this one (this launch is too big).
// Refs: R-H212, NFR-17.5, FR-17.26
func (p SandboxPolicy) EnforceResourceLimits(o SandboxLaunchOptions) error {
	for _, d := range p.dimensions(o) {
		if d.limit <= 0 || d.requested <= d.limit {
			continue
		}
		return fmt.Errorf("%w: requested %s %d %s exceeds the per-sandbox maximum of %d %s "+
			"set by host sandbox policy %s; ask for at most %d with %s, or have the operator raise %s "+
			"(mgit refuses rather than silently giving you less than you asked for)",
			ErrSandboxResourceLimitExceeded, d.field, d.requested, d.unit, d.limit, d.unit,
			d.maxField, d.limit, d.flag, d.maxField)
	}
	return nil
}
