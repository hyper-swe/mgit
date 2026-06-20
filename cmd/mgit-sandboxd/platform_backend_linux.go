//go:build linux

package main

import (
	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/backend/firecracker"
)

// newHypervisorBackend wires the Linux KVM/Firecracker microVM backend
// (MGIT-11.5.1). It fails closed when the firecracker binary or /dev/kvm
// is absent — a missing hypervisor is a hard error, never a silent
// downgrade (FR-17.15). Refs: FR-17.15, FR-17.16
func newHypervisorBackend(deps hypervisorDeps) (model.SandboxManager, error) {
	return firecracker.NewManager(firecracker.Config{
		WorkDir:    deps.workDir,
		Resolve:    newImageResolver(deps.hostRoot, deps.clock),
		Logger:     deps.logger,
		Clock:      deps.clock,
		PeerBinder: deps.peerBinder,
	})
}
