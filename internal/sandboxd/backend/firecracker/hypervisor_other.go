//go:build !linux

package firecracker

import (
	"fmt"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/backend/microvm"
)

// newPlatformHypervisor fails closed off Linux: Firecracker is a
// KVM-only VMM, so the kvm backend simply does not exist here.
// Refs: FR-17.15
func newPlatformHypervisor(_ string) (microvm.Hypervisor, error) {
	return nil, fmt.Errorf("%w: kvm requires linux with /dev/kvm", model.ErrSandboxBackendUnavailable)
}
