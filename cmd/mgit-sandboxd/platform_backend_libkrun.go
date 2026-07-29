//go:build cgo && !vzf && (darwin || (linux && libkrun))

package main

import (
	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/backend/libkrun"
	"github.com/hyper-swe/mgit/internal/sandboxd/backend/microvm"
)

// newHypervisorBackend wires the libkrun cross-platform microVM backend
// (ADR-010, MGIT-61.8). The selection is a BUILD-TIME fact, and it is now
// ASYMMETRIC per the GA position (2026-07-29, ADR-010): on darwin+cgo this
// file compiles by DEFAULT (no -tags libkrun needed — vzf there requires the
// explicit -tags vzf opt-out); on linux it still requires the explicit
// -tags libkrun opt-in, because firecracker remains the Linux GA default
// (libkrun's real-VM boot does not yet complete on Linux/KVM). A build-time
// fact deliberately stays out of the FR-17.15 runtime backend contract —
// there is no runtime flag to pick between two linked hypervisors, so no
// silent-downgrade path exists. The daemon logs which VMM it linked so an
// operator can always tell.
//
// SEC-03: the provisioner is wired fail-closed exactly like the other
// backends, and the libkrun backend now DELIVERS the quarantined staging
// tree (CreateVM builds it from WorktreePath + PrivateStorePath rather than
// refusing — MGIT-61.13 P7).
func newHypervisorBackend(deps hypervisorDeps) (model.SandboxManager, microvm.GuestDialer, error) {
	prov, err := newStoreProvisioner(deps)
	if err != nil {
		return nil, nil, err
	}
	deps.logger.Info("sandbox VMM linked at build time",
		"event", "vmm_linked", "vmm", model.BackendLibkrun,
		"detail", "libkrun re-exec child per VM; vzf/firecracker not selectable in this build")
	mgr, err := libkrun.NewManager(libkrun.Config{
		WorkDir:          deps.workDir,
		Resolve:          newImageResolver(deps.hostRoot, deps.clock),
		Logger:           deps.logger,
		Clock:            deps.clock,
		PeerBinder:       deps.peerBinder,
		NotifyRegistrar:  deps.notifyReg,
		StoreProvisioner: prov,
		SensitivePaths:   model.DefaultSandboxPolicy().SensitivePaths,
	})
	if err != nil {
		return nil, nil, err
	}
	return mgr, libkrun.NewLandDialer(deps.workDir), nil
}
