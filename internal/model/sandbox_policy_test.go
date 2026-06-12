// Package model tests verify the SandboxPolicy type per MGIT-11.3.4
// acceptance criteria. Refs: FR-17.13, FR-17.6
package model

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestDefaultSandboxPolicy_SafeDefaults pins the FR-17 safe defaults.
// Refs: FR-17.6, FR-17.14, NFR-17.5
func TestDefaultSandboxPolicy_SafeDefaults(t *testing.T) {
	p := DefaultSandboxPolicy()

	assert.True(t, p.RequireSandbox, "require_sandbox defaults true (FR-17.6)")
	assert.Equal(t, NetworkModeAllowlist, p.Network.Mode, "default network is allowlist, never open")
	assert.Contains(t, p.SensitivePaths, ".claude/", "host-trusted paths covered (FR-17.14)")
	assert.Contains(t, p.SensitivePaths, ".git/hooks/")
	assert.Equal(t, 2, p.CPUs)
	assert.Equal(t, 2048, p.MemoryMB)
	assert.Equal(t, 4096, p.DiskQuotaMB)
	assert.Equal(t, 4*time.Hour, p.TTL)
	assert.NoError(t, p.Validate(), "defaults must validate")
}

// TestSandboxPolicy_Validate covers the error paths. Refs: FR-17.13
func TestSandboxPolicy_Validate(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*SandboxPolicy)
	}{
		{name: "unknown_network_mode", mutate: func(p *SandboxPolicy) { p.Network.Mode = "bridge" }},
		{name: "empty_sensitive_path_entry", mutate: func(p *SandboxPolicy) { p.SensitivePaths = []string{""} }},
		{name: "negative_cpus", mutate: func(p *SandboxPolicy) { p.CPUs = -1 }},
		{name: "negative_memory", mutate: func(p *SandboxPolicy) { p.MemoryMB = -1 }},
		{name: "negative_disk", mutate: func(p *SandboxPolicy) { p.DiskQuotaMB = -1 }},
		{name: "negative_ttl", mutate: func(p *SandboxPolicy) { p.TTL = -time.Second }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := DefaultSandboxPolicy()
			tt.mutate(&p)
			assert.Error(t, p.Validate())
		})
	}
}
