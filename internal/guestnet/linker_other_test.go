//go:build !linux

package guestnet

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/guestboot"
)

// TestNewLinker_NonLinux_IsNil verifies a non-Linux build supplies no link
// configurator, so Apply fails closed rather than reporting success for a NIC
// it never touched. The sandbox guest is always Linux (ADR-005); this build
// exists only so the package compiles on a developer's host.
// Refs: MGIT-68, ADR-005
func TestNewLinker_NonLinux_IsNil(t *testing.T) {
	assert.Nil(t, NewLinker())
}

// TestApply_NonLinuxBuild_RefusesRatherThanPretends pins the consequence: a
// valid descriptor on a platform with no configurator is an error, never a
// silent success.
func TestApply_NonLinuxBuild_RefusesRatherThanPretends(t *testing.T) {
	err := Apply(guestboot.GuestNetwork{IP: "10.0.2.15", PrefixLen: 24, Gateway: "10.0.2.2"},
		Deps{Link: NewLinker(), ResolvPath: t.TempDir() + "/resolv.conf"})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no link configurator")
}
