//go:build linux

package main

import (
	"os"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/backend/firecracker"
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
		WorkDir:    deps.workDir,
		Resolve:    newImageResolver(deps.hostRoot, deps.clock),
		Logger:     deps.logger,
		Clock:      deps.clock,
		ExtIface:   os.Getenv(extIfaceEnv),
		PeerBinder: deps.peerBinder,
	})
}
