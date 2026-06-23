//go:build linux

package main

import (
	"os"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/backend/firecracker"
	"github.com/hyper-swe/mgit/internal/sandboxd/backend/microvm"
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
// auto-detected. It also returns the firecracker host LAND dialer (the
// per-VM vsock socket under workDir, microvm.GuestLandPort) so the daemon
// land wiring selects the transport by active backend. Refs: FR-17.15, FR-17.16, FR-17.7, FR-17.5
func newHypervisorBackend(deps hypervisorDeps) (model.SandboxManager, microvm.GuestDialer, error) {
	// SEC-03 fail-closed: the Linux/KVM backend delivers the quarantine, so it
	// refuses to construct without a shared store to seed the per-task private
	// store from. A microVM must never boot unquarantined (pre-SEC-03 delivery
	// would expose the worktree's own store to the guest) — better no sandbox
	// than a silently-degraded one. Refs: SEC-03, MGIT-11.6.8
	prov, err := newStoreProvisioner(deps)
	if err != nil {
		return nil, nil, err
	}
	mgr, err := firecracker.NewManager(firecracker.Config{
		WorkDir:          deps.workDir,
		Resolve:          newImageResolver(deps.hostRoot, deps.clock),
		Logger:           deps.logger,
		Clock:            deps.clock,
		ExtIface:         os.Getenv(extIfaceEnv),
		PeerBinder:       deps.peerBinder,
		NotifyRegistrar:  deps.notifyReg,
		StoreProvisioner: prov,
		SensitivePaths:   model.DefaultSandboxPolicy().SensitivePaths,
	})
	if err != nil {
		return nil, nil, err
	}
	// The firecracker land dialer is stateless host I/O (reconstructs the
	// per-VM vsock socket path from workDir + sandbox ID), so it is built
	// independently of the manager. Refs: FR-17.5
	return mgr, firecracker.NewLandDialer(deps.workDir), nil
}
