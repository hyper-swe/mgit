package hyperv

import (
	"fmt"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/backend/microvm"
)

// newPlatformHost fails closed: no Windows sandbox backend ships in v1
// (ADR-006, FR-17.39). The decision (MGIT-11.5.3) resolved to a
// host-matching WINDOWS guest via a Hyper-V-isolated Windows container
// (WCOW) rather than a Linux guest — the LCOW path was rejected because
// hcsshim exposes the Linux utility-VM lifecycle only under internal/
// (not importable) and it is deprecated on Windows clients. The WCOW
// backend is a distinct model (HCS/containerd + a Windows guest agent),
// built in epic MGIT-12; until then this returns an honest unavailable
// error rather than a fabricated implementation. Refs: FR-17.39, ADR-006, MGIT-12
func newPlatformHost() (microvm.Hypervisor, error) {
	return nil, fmt.Errorf(
		"%w: native Windows sandbox deferred to v1+ (WCOW backend, MGIT-12); "+
			"Windows runs core mgit without the microVM sandbox",
		model.ErrSandboxBackendUnavailable)
}
