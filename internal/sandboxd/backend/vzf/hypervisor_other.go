//go:build !darwin || !cgo

package vzf

import (
	"fmt"

	"github.com/hyper-swe/mgit/internal/model"
)

// newPlatformHypervisor fails closed on builds without the
// Virtualization.framework bindings (non-darwin, or CGO disabled):
// the vzf backend simply does not exist here. Refs: FR-17.15
func newPlatformHypervisor() (hypervisor, error) {
	return nil, fmt.Errorf("%w: vzf requires darwin with CGO", model.ErrSandboxBackendUnavailable)
}
