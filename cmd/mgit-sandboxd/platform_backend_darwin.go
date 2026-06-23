//go:build darwin

package main

import (
	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/backend/microvm"
	"github.com/hyper-swe/mgit/internal/sandboxd/backend/vzf"
)

// newHypervisorBackend wires the macOS Virtualization.framework microVM
// backend (MGIT-11.5.2). It fails closed on builds without the vz
// bindings (CGO off) — a missing hypervisor is a hard error, never a
// silent downgrade (FR-17.15). Refs: FR-17.15, FR-17.16
//
// SEC-03: the private-store quarantine IS delivered on vzf (MGIT-11.6.9). vzf
// shares a live host dir over virtiofs, so the shared microvm manager seeds the
// per-task private store + builds the plan + fails closed, and the vz
// hypervisor shares a STAGED worktree (worktree files + the private .mgit, the
// host shared store excluded, escaping symlinks rejected) rather than the live
// worktree (see worktreeShare). This wiring fails CLOSED without a repo root to
// seed from, exactly like the Linux/firecracker path — better no sandbox than a
// silently-unquarantined one. Refs: SEC-03, MGIT-11.6.9
//
// It also returns the vzf host LAND dialer (MGIT-13.1.1): unlike firecracker's
// stateless socket-path dialer, the vzf land dialer must resolve a sandbox to
// its live VZVirtualMachine, so it is built by NewManagerWithLand bound to the
// SAME live-VM registry the manager's hypervisor publishes into, and connects
// on microvm.GuestLandPort over VZVirtioSocketDevice.Connect. The daemon land
// wiring uses it behind the microvm.GuestDialer seam. Refs: FR-17.5, FR-17.16, MGIT-13.1.1
func newHypervisorBackend(deps hypervisorDeps) (model.SandboxManager, microvm.GuestDialer, error) {
	prov, err := newStoreProvisioner(deps)
	if err != nil {
		return nil, nil, err
	}
	mgr, landDialer, err := vzf.NewManagerWithLand(vzf.Config{
		WorkDir:          deps.workDir,
		Resolve:          newImageResolver(deps.hostRoot, deps.clock),
		Logger:           deps.logger,
		Clock:            deps.clock,
		PeerBinder:       deps.peerBinder,
		StoreProvisioner: prov,
		SensitivePaths:   model.DefaultSandboxPolicy().SensitivePaths,
	})
	if err != nil {
		return nil, nil, err
	}
	return mgr, landDialer, nil
}
