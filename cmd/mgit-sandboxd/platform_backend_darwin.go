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
// SEC-03: the private-store quarantine IS delivered on vzf (MGIT-11.6.9). vzf
// shares a live host dir over virtiofs, so the shared microvm manager seeds the
// per-task private store + builds the plan + fails closed, and the vz
// hypervisor shares a STAGED worktree (worktree files + the private .mgit, the
// host shared store excluded, escaping symlinks rejected) rather than the live
// worktree (see worktreeShare). This wiring fails CLOSED without a repo root to
// seed from, exactly like the Linux/firecracker path — better no sandbox than a
// silently-unquarantined one. Refs: SEC-03, MGIT-11.6.9
func newHypervisorBackend(deps hypervisorDeps) (model.SandboxManager, error) {
	prov, err := newStoreProvisioner(deps)
	if err != nil {
		return nil, err
	}
	return vzf.NewManager(vzf.Config{
		WorkDir:          deps.workDir,
		Resolve:          newImageResolver(deps.hostRoot, deps.clock),
		Logger:           deps.logger,
		Clock:            deps.clock,
		PeerBinder:       deps.peerBinder,
		StoreProvisioner: prov,
		SensitivePaths:   model.DefaultSandboxPolicy().SensitivePaths,
	})
}
