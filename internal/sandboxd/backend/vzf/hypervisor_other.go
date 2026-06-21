//go:build !darwin || !cgo

package vzf

import (
	"fmt"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/backend/microvm"
)

// newPlatformHypervisor fails closed on builds without the
// Virtualization.framework bindings (non-darwin, or CGO disabled):
// the vzf backend simply does not exist here. The registry is accepted
// for signature parity with the darwin build and ignored. Refs: FR-17.15
func newPlatformHypervisor(_ *liveVMs) (microvm.Hypervisor, error) {
	return nil, fmt.Errorf("%w: vzf requires darwin with CGO", model.ErrSandboxBackendUnavailable)
}
