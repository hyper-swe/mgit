// Package model defines pure domain types for mgit.
// These tests verify the host-derived capability-escalation model types
// (ObservedDenial -> CapabilityRequest, CapabilityGrant) per MGIT-11.9.4.
// Refs: FR-17.12, SEC-05
package model

import (
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestObservedDenial_RequestFromObservedDenial_HostDerived proves the request
// is built from host-observed facts alone (SEC-05): the struct carries no
// guest field, and the produced request validates.
func TestObservedDenial_RequestFromObservedDenial_HostDerived(t *testing.T) {
	t.Parallel()

	d := ObservedDenial{
		SandboxID: "01SB", TaskID: "MGIT-11.9.4",
		DestIP: netip.MustParseAddr("203.0.113.7"), DestPort: 443,
		Rule: "raw-ip not allowlisted",
	}
	req, err := d.RequestFromObservedDenial()
	require.NoError(t, err)
	assert.Equal(t, CapabilityEgress, req.Capability)
	assert.Equal(t, "203.0.113.7", req.ObservedDestIP)
	assert.Equal(t, 443, req.ObservedDestPort)
	assert.Equal(t, "MGIT-11.9.4", req.TaskID)
}

// TestObservedDenial_RequestFromObservedDenial_RejectsInvalid proves a denial
// with no/invalid host-observed destination yields an error rather than a
// forgeable request.
func TestObservedDenial_RequestFromObservedDenial_RejectsInvalid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		d    ObservedDenial
	}{
		{name: "no_ip", d: ObservedDenial{SandboxID: "01SB", TaskID: "MGIT-1", DestPort: 443}},
		{name: "no_port", d: ObservedDenial{SandboxID: "01SB", TaskID: "MGIT-1", DestIP: netip.MustParseAddr("203.0.113.7")}},
		{name: "no_task", d: ObservedDenial{SandboxID: "01SB", DestIP: netip.MustParseAddr("203.0.113.7"), DestPort: 443}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := tt.d.RequestFromObservedDenial()
			require.Error(t, err)
		})
	}
}

// TestCapabilityGrant_AllowlistEntry_SingleHost proves an egress grant renders
// as an exact ip:port allowlist entry — never a range/wildcard (SEC-05).
func TestCapabilityGrant_AllowlistEntry_SingleHost(t *testing.T) {
	t.Parallel()

	g := CapabilityGrant{
		ObservedDestIP: "198.51.100.5", ObservedDestPort: 8443,
		Scope: GrantScopeSandboxLifetime,
	}
	assert.Equal(t, "198.51.100.5:8443", g.AllowlistEntry())
	assert.Equal(t, GrantScopeSandboxLifetime, g.Scope)
}
