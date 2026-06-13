//go:build darwin && cgo

// Real-vz construction tests: an UNSIGNED test binary can build every
// Virtualization.framework configuration object; only Validate (and
// boot) require the com.apple.security.virtualization entitlement.
// These tests therefore exercise the full CreateVM construction path
// and accept exactly two outcomes: a created VM (entitled runner) or
// the framework's entitlement error from Validate — anything else is
// a construction bug. Booting a real guest is the e2e suite's job
// (MGIT-11.13.1). Refs: FR-17.15
package vzf

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/sandboxd/backend/microvm"
)

func probeConfig(t *testing.T, attachNIC bool) microvm.VMConfig {
	t.Helper()
	dir := t.TempDir()
	kernel := filepath.Join(dir, "vmlinux")
	rootfs := filepath.Join(dir, "rootfs.img")
	overlay := filepath.Join(dir, "overlay.img")
	for _, path := range []string{kernel, rootfs, overlay} {
		require.NoError(t, os.WriteFile(path, make([]byte, 1024), 0o600))
	}
	return microvm.VMConfig{
		CPUs: 2, MemoryMB: 1024,
		KernelPath: kernel, RootfsPath: rootfs, RootfsReadOnly: true,
		Cmdline:     "console=hvc0 root=/dev/vda ro",
		OverlayPath: overlay, WorktreePath: dir, WorktreeTag: "work",
		AttachNIC: attachNIC, VsockEnabled: true, BalloonEnabled: true,
	}
}

// TestVZHypervisor_ConstructsFullConfiguration drives the real vz
// constructors for both network modes. Refs: FR-17.3, FR-17.7, FR-17.15
func TestVZHypervisor_ConstructsFullConfiguration(t *testing.T) {
	hv, err := newPlatformHypervisor()
	require.NoError(t, err)

	for _, attachNIC := range []bool{false, true} {
		vm, err := hv.CreateVM(probeConfig(t, attachNIC))
		if err == nil {
			assert.NotNil(t, vm, "entitled runner: VM created")
			continue
		}
		// Unsigned test binary: every constructor must have succeeded,
		// with the only failure being the entitlement check in Validate.
		assert.Contains(t, err.Error(), "vz configuration invalid",
			"the only acceptable failure is the entitlement validation, got: %v", err)
		assert.Contains(t, err.Error(), "com.apple.security.virtualization",
			"failure must be the entitlement, not a construction bug: %v", err)
	}
}

// TestVZHypervisor_BadImagePathsSurface covers the construction error
// paths: missing image files fail at attachment, before validation.
func TestVZHypervisor_BadImagePathsSurface(t *testing.T) {
	hv, err := newPlatformHypervisor()
	require.NoError(t, err)

	cfg := probeConfig(t, false)
	cfg.RootfsPath = filepath.Join(t.TempDir(), "absent.img")
	_, err = hv.CreateVM(cfg)
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "rootfs attachment"),
		"a missing rootfs must fail at the attachment step, got: %v", err)

	cfg = probeConfig(t, false)
	cfg.OverlayPath = filepath.Join(t.TempDir(), "absent-overlay.img")
	_, err = hv.CreateVM(cfg)
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "overlay attachment"),
		"a missing overlay must fail at the attachment step, got: %v", err)
}
