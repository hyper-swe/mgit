//go:build libkrun && cgo && (darwin || linux)

package main

import (
	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/backend/libkrun"
	"github.com/hyper-swe/mgit/internal/sandboxd/backend/microvm"
)

// newHypervisorBackend wires the libkrun cross-platform microVM backend
// (ADR-010, MGIT-61.8). The selection is a BUILD-TIME fact: a daemon built
// with -tags libkrun links libkrun and uses it on darwin AND linux; the
// default build keeps vzf/firecracker. A build-time fact deliberately stays
// out of the FR-17.15 runtime backend contract — there is no runtime flag to
// pick between two linked hypervisors, so no silent-downgrade path exists.
// The daemon logs which VMM it linked so an operator can always tell.
//
// SEC-03: the provisioner is wired fail-closed exactly like the other
// backends — but the libkrun backend cannot DELIVER the quarantined layout
// yet, so launches carrying a private store are refused by CreateVM with the
// reason (MGIT-61.6). Better a refused sandbox than an unquarantined one.
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
