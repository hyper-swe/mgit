package model

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// limitOpts builds valid launch options carrying the declared resource caps.
func limitOpts(cpus, memoryMB, diskMB int) SandboxLaunchOptions {
	return SandboxLaunchOptions{
		TaskID: "MGIT-95", WorktreePath: "/work/a",
		ImageRef: "base@sha256:" + strings.Repeat("b", 64),
		Network:  NetworkPolicy{Mode: NetworkModeNone},
		CPUs:     cpus, MemoryMB: memoryMB, DiskQuotaMB: diskMB,
	}
}

// TestDefaultSandboxPolicy_PerSandboxMaxima_ExceedTheDefaults verifies the
// shipped policy carries a per-sandbox maximum for every resource dimension
// and that each maximum leaves real headroom above the default a launch gets
// when it declares nothing. A default equal to the maximum would make the
// declarable surface useless. Refs: R-H212, NFR-17.5
func TestDefaultSandboxPolicy_PerSandboxMaxima_ExceedTheDefaults(t *testing.T) {
	p := DefaultSandboxPolicy()
	assert.Greater(t, p.MaxCPUs, p.CPUs, "per-sandbox CPU maximum must exceed the default")
	assert.Greater(t, p.MaxMemoryMB, p.MemoryMB, "per-sandbox memory maximum must exceed the default")
	assert.Greater(t, p.MaxDiskQuotaMB, p.DiskQuotaMB, "per-sandbox disk maximum must exceed the default")
	// The motivating measurement: a real production build peaks at 2.10 GB RSS
	// and must be declarable without an operator editing host policy (R-H212).
	assert.GreaterOrEqual(t, p.MaxMemoryMB, 4096,
		"the default policy must let a caller declare enough for a 2.10 GB build")
}

// TestEnforceResourceLimits_WithinBound_Accepted verifies a declared request
// at or below the per-sandbox maximum passes. Refs: R-H212
func TestEnforceResourceLimits_WithinBound_Accepted(t *testing.T) {
	p := DefaultSandboxPolicy()
	tests := []struct {
		name string
		opts SandboxLaunchOptions
	}{
		{name: "all_unset_takes_defaults", opts: limitOpts(0, 0, 0)},
		{name: "under_the_bound", opts: limitOpts(4, 4096, 8192)},
		{name: "exactly_at_the_bound", opts: limitOpts(p.MaxCPUs, p.MaxMemoryMB, p.MaxDiskQuotaMB)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.NoError(t, p.EnforceResourceLimits(tt.opts))
		})
	}
}

// TestEnforceResourceLimits_OverBound_RefusedNamingTheLimit verifies an
// over-bound request is REFUSED — never clamped — and that the refusal names
// the requested value, the limit, and the policy field that set it, so the
// caller can act on it instead of reshaping its workload. Refs: R-H212
func TestEnforceResourceLimits_OverBound_RefusedNamingTheLimit(t *testing.T) {
	p := DefaultSandboxPolicy()
	p.MaxCPUs, p.MaxMemoryMB, p.MaxDiskQuotaMB = 4, 4096, 8192

	tests := []struct {
		name       string
		opts       SandboxLaunchOptions
		wantField  string
		wantLimit  string
		wantAmount string
	}{
		{
			name: "memory_over_bound", opts: limitOpts(0, 8192, 0),
			wantField: "max_memory_mb", wantLimit: "4096", wantAmount: "8192",
		},
		{
			name: "cpus_over_bound", opts: limitOpts(16, 0, 0),
			wantField: "max_cpus", wantLimit: "4", wantAmount: "16",
		},
		{
			name: "disk_over_bound", opts: limitOpts(0, 0, 65536),
			wantField: "max_disk_quota_mb", wantLimit: "8192", wantAmount: "65536",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := p.EnforceResourceLimits(tt.opts)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrSandboxResourceLimitExceeded)
			assert.NotErrorIs(t, err, ErrSandboxCeilingExceeded,
				"a too-big single launch is not the fleet-is-full refusal")
			msg := err.Error()
			assert.Contains(t, msg, tt.wantAmount, "the refusal names what was requested")
			assert.Contains(t, msg, tt.wantLimit, "the refusal names the limit")
			assert.Contains(t, msg, tt.wantField, "the refusal names the policy field that set it")
			assert.Contains(t, msg, "host sandbox policy", "the refusal names where the limit lives")
		})
	}
}

// TestEnforceResourceLimits_ZeroMaximum_Unbounded verifies a zero maximum
// disables that dimension's per-sandbox bound (matching the existing
// zero-disables convention of the aggregate ceilings). The aggregate ceiling
// still applies on top. Refs: R-H212, FR-17.26
func TestEnforceResourceLimits_ZeroMaximum_Unbounded(t *testing.T) {
	p := DefaultSandboxPolicy()
	p.MaxCPUs, p.MaxMemoryMB, p.MaxDiskQuotaMB = 0, 0, 0
	assert.NoError(t, p.EnforceResourceLimits(limitOpts(256, 1<<20, 1<<20)))
}

// TestSandboxPolicy_Validate_MaximaBelowDefaults_Rejected verifies a policy
// whose default exceeds its own per-sandbox maximum is refused: it would make
// every undeclared launch instantly illegal. Refs: R-H212, FR-17.13
func TestSandboxPolicy_Validate_MaximaBelowDefaults_Rejected(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*SandboxPolicy)
		wantErr string
	}{
		{
			name:    "memory_default_over_max",
			mutate:  func(p *SandboxPolicy) { p.MemoryMB, p.MaxMemoryMB = 8192, 4096 },
			wantErr: "max_memory_mb",
		},
		{
			name:    "cpu_default_over_max",
			mutate:  func(p *SandboxPolicy) { p.CPUs, p.MaxCPUs = 8, 4 },
			wantErr: "max_cpus",
		},
		{
			name:    "disk_default_over_max",
			mutate:  func(p *SandboxPolicy) { p.DiskQuotaMB, p.MaxDiskQuotaMB = 8192, 4096 },
			wantErr: "max_disk_quota_mb",
		},
		{
			name:    "negative_maximum",
			mutate:  func(p *SandboxPolicy) { p.MaxMemoryMB = -1 },
			wantErr: "max_memory_mb",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := DefaultSandboxPolicy()
			tt.mutate(&p)
			err := p.Validate()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
			var vErr *ValidationError
			assert.True(t, errors.As(err, &vErr), "policy shape failures are ValidationErrors")
		})
	}
}
