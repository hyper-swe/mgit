//go:build linux

package main

import (
	"fmt"
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
	// SEC-03 fail-closed: the Linux/KVM backend delivers the quarantine, so it
	// refuses to construct without a shared store to seed the per-task private
	// store from. A microVM must never boot unquarantined (pre-SEC-03 delivery
	// would expose the worktree's own store to the guest) — better no sandbox
	// than a silently-degraded one. Refs: SEC-03, MGIT-11.6.8
	prov, err := newStoreProvisioner(deps)
	if err != nil {
		return nil, err
	}
	return firecracker.NewManager(firecracker.Config{
		WorkDir:          deps.workDir,
		Resolve:          newImageResolver(deps.hostRoot, deps.clock),
		Logger:           deps.logger,
		Clock:            deps.clock,
		ExtIface:         os.Getenv(extIfaceEnv),
		PeerBinder:       deps.peerBinder,
		StoreProvisioner: prov,
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
// resolved repo root. It is an ERROR (fail closed) when no repo root is known
// or the provisioner cannot be built: the quarantine control cannot be realized
// without a shared store to seed from, and the caller refuses to bring up an
// unquarantined sandbox backend rather than silently degrading. Refs: SEC-03
func newStoreProvisioner(deps hypervisorDeps) (provision.Provisioner, error) {
	root := resolveRepoRoot(deps)
	if root == "" {
		return nil, fmt.Errorf("%w: SEC-03 quarantine requires a repo root to seed the private store "+
			"(set --repo-root or a host config root); refusing to launch sandboxes unquarantined",
			model.ErrSandboxBackendUnavailable)
	}
	p, err := provision.NewStoreProvisioner(root)
	if err != nil {
		return nil, fmt.Errorf("%w: SEC-03 private-store provisioner: %w", model.ErrSandboxBackendUnavailable, err)
	}
	return p, nil
}
