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
	// Global ceilings enforced by mgit-sandboxd across all sandboxes
	// (FR-17.26): exceeding either fails launch rather than degrading
	// the host.
	MaxConcurrentSandboxes int `json:"max_concurrent_sandboxes"`
	MaxTotalMemoryPercent  int `json:"max_total_memory_percent"`
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
	return nil
}
