//go:build linux

package main

import (
	"os"
	"path/filepath"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/backend/firecracker"
	"github.com/hyper-swe/mgit/internal/sandboxd/provision"
)

// extIfaceEnv overrides open-mode NAT egress to a specific host interface.
// Empty (the default) auto-detects the host default-route interface; set it
// only on a multi-homed host that must NAT through a non-default link.
// Refs: FR-17.7
const extIfaceEnv = "MGIT_SANDBOX_EXT_IFACE"

// newHypervisorBackend wires the Linux KVM/Firecracker microVM backend
// (MGIT-11.5.1). It fails closed when the firecracker binary or /dev/kvm
// is absent — a missing hypervisor is a hard error, never a silent
// downgrade (FR-17.15). The open-mode external interface comes from
// MGIT_SANDBOX_EXT_IFACE when set, else the default-route interface is
// auto-detected. Refs: FR-17.15, FR-17.16, FR-17.7
func newHypervisorBackend(deps hypervisorDeps) (model.SandboxManager, error) {
	return firecracker.NewManager(firecracker.Config{
		WorkDir:          deps.workDir,
		Resolve:          newImageResolver(deps.hostRoot, deps.clock),
		Logger:           deps.logger,
		Clock:            deps.clock,
		ExtIface:         os.Getenv(extIfaceEnv),
		PeerBinder:       deps.peerBinder,
		StoreProvisioner: newStoreProvisioner(deps),
		SensitivePaths:   model.DefaultSandboxPolicy().SensitivePaths,
	})
}

// resolveRepoRoot returns the mgit repo root for SEC-03 provisioning: the
// explicit --repo-root, else the conventional parent of the host config root
// (<repo>/.mgit/sandbox -> <repo>). Mirrors buildLandService's fallback.
func resolveRepoRoot(deps hypervisorDeps) string {
	if deps.repoRoot != "" {
		return deps.repoRoot
	}
	if deps.hostRoot == "" {
		return ""
	}
	return filepath.Dir(filepath.Dir(deps.hostRoot))
}

// newStoreProvisioner builds the SEC-03 private-store provisioner from the
// resolved repo root, or returns nil (logging a warning) when no repo root is
// known — in which case the quarantine control cannot be realized and the
// manager's nil-provisioner path applies. Returning nil is honest: the daemon
// has no shared store to seed from. Refs: SEC-03
func newStoreProvisioner(deps hypervisorDeps) provision.Provisioner {
	root := resolveRepoRoot(deps)
	if root == "" {
		deps.logger.Warn("SEC-03 private-store provisioning disabled: no repo root",
			"event", "quarantine_unprovisioned")
		return nil
	}
	p, err := provision.NewStoreProvisioner(root)
	if err != nil {
		deps.logger.Warn("SEC-03 private-store provisioning disabled",
			"event", "quarantine_unprovisioned", "error", err.Error())
		return nil
	}
	return p
}
