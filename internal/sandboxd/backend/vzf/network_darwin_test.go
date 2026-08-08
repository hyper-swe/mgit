//go:build darwin && cgo

package vzf

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// TestRefuseUnenforceableNetwork_AllowlistIsRefused is MGIT-70's honesty
// fix: vzf attaches a macOS NAT device with no tap, firewall, proxy or
// pinning resolver, and wireEgress is a no-op off Linux — so allowlist mode
// here granted UNRESTRICTED egress while naming a policy. That is SEC-04
// false containment, and refusing it is the same call the container backend
// already makes. Refs: MGIT-70, SEC-04, FR-17.7
func TestRefuseUnenforceableNetwork_AllowlistIsRefused(t *testing.T) {
	err := refuseUnenforceableNetwork(model.NetworkModeAllowlist)

	require.Error(t, err)
	require.ErrorIs(t, err, model.ErrNetworkPolicyViolation)
	assert.Contains(t, err.Error(), "not enforceable")
	assert.Contains(t, err.Error(), "libkrun", "the error must name the backend that CAN enforce it")
}

// TestRefuseUnenforceableNetwork_NoneAndOpenAreHonest verifies the modes vzf
// can serve truthfully are untouched: none attaches no NIC, and open is
// unrestricted by definition, so NAT is an accurate implementation of it.
func TestRefuseUnenforceableNetwork_NoneAndOpenAreHonest(t *testing.T) {
	assert.NoError(t, refuseUnenforceableNetwork(model.NetworkModeNone))
	assert.NoError(t, refuseUnenforceableNetwork(model.NetworkModeOpen))
}
