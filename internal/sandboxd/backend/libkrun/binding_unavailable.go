//go:build !cgo || vzf || (!darwin && !(linux && libkrun))

package libkrun

import (
	"fmt"

	"github.com/hyper-swe/mgit/internal/model"
)

// newPlatformAPI reports that this build cannot drive libkrun.
//
// The libkrun backend is opt-in at BUILD time: linking it requires the
// libkrun shared library and CGO, while core mgit and the firecracker daemon
// are deliberately pure Go so `go install` and the release builds keep
// working everywhere. A build without the tag therefore has the backend's
// pure-Go logic (and its tests) but no binding — and says so, instead of
// failing later with a link or load error. Refs: FR-17.15, ADR-010
func newPlatformAPI() (krunAPI, error) {
	return nil, fmt.Errorf(
		"%w: this mgit-sandboxd was built without libkrun support; "+
			"rebuild with -tags libkrun (needs CGO and the libkrun library), "+
			"or use the firecracker/vzf backend",
		model.ErrSandboxBackendUnavailable)
}

// newCapabilityProbe returns no probe in a build without the libkrun binding.
// There is nothing linked to interrogate, and newPlatformAPI already refuses
// such a build with its own actionable message. Refs: MGIT-61.14
func newCapabilityProbe() netCapabilityProbe { return nil }
