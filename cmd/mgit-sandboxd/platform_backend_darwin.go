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
func newHypervisorBackend(deps hypervisorDeps) (model.SandboxManager, error) {
	return vzf.NewManager(vzf.Config{
		WorkDir: deps.workDir,
		Resolve: newImageResolver(deps.hostRoot, deps.clock),
		Logger:  deps.logger,
		Clock:   deps.clock,
	})
}
