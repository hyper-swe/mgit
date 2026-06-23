//go:build darwin

package main

import (
	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/backend/vzf"
)

// newHypervisorBackend wires the macOS Virtualization.framework microVM
// backend (MGIT-11.5.2). It fails closed on builds without the vz
// bindings (CGO off) — a missing hypervisor is a hard error, never a
// silent downgrade (FR-17.15). Refs: FR-17.15, FR-17.16
//
// SEC-03 NOTE: the SEC-03 private-store quarantine is NOT yet wired for vzf.
// vzf shares the live worktree dir over virtiofs and has no copy-and-land
// image-build seam to lay a private store into; wiring it requires a virtiofs
// delivery that mounts the private store at the guest .mgit and excludes the
// shared one (MGIT-11.6.8 SLICE 4 deferral). Until then the provisioner is
// deliberately NOT passed here, so the microvm manager's quarantine step is a
// no-op on macOS — the documented pre-SEC-03 behavior, NOT a faked control.
// The proven KVM/firecracker backend (platform_backend_linux.go) realizes
// SEC-03 fully. Refs: SEC-03, MGIT-11.6.8
func newHypervisorBackend(deps hypervisorDeps) (model.SandboxManager, error) {
	return vzf.NewManager(vzf.Config{
		WorkDir:    deps.workDir,
		Resolve:    newImageResolver(deps.hostRoot, deps.clock),
		Logger:     deps.logger,
		Clock:      deps.clock,
		PeerBinder: deps.peerBinder,
	})
}
